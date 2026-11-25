// DoH 路径转发代理（Go 版，仅用标准库）
//
// 与仓库根目录的 Cloudflare Worker / EdgeOne 中间件实现路由逻辑一致：
//
//	/<上游>/query-dns  → 该上游的 DoH 端点，保留查询串与请求体
//	/<上游>/dns-query  → 同上（路径透传）
//	/dns-query、/query-dns、/（仅 DoH 请求）→ DEFAULT_UPSTREAM
//	其它路径 → 伪装 404 页面
//
// 环境变量：
//
//	LISTEN_ADDR       监听地址，默认 :8080
//	TLS_LISTEN        额外的 TLS 监听地址（如 :8444）；留空则 -tls-cert 生效时直接把 LISTEN_ADDR 变成 HTTPS
//	DOMAIN_MAPPINGS   覆盖整张映射表（JSON，字段同 Worker 版）
//	ALLOWED_HOSTS     允许的 Host，逗号分隔；为空表示不限制
//	DEFAULT_UPSTREAM  裸路径默认上游，默认 cloudflare
//	CACHE_TTL         GET+application/dns-message 的缓存秒数，默认 0（不缓存）
//	VERBOSE=1         打印每条请求日志
//	HEALTH_PATH       健康检查路径，默认空（关闭）
//	TLS_CERT/TLS_KEY  证书与私钥路径；配置后按上面的规则启用 HTTPS
package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxBodyBytes = 64 << 10 // DoH 报文很小，64KB 足够

// Mapping 一条路径前缀映射
type Mapping struct {
	TargetDomain string            `json:"targetDomain"`
	PathMapping  map[string]string `json:"pathMapping"`
}

// Config 运行时配置
type Config struct {
	Mappings        map[string]Mapping
	AllowedHosts    []string
	DefaultUpstream string
	CacheTTL        int
	Verbose         bool
	HealthPath      string
	MetricsPath     string

	// 并发与资源限制（对应 nginx 的 worker_connections / keepalive / 超时族）
	MaxInFlight     int // 全局在途请求上限，满则 503（worker_connections 的作用）
	MaxConnsPerHost int // 单上游连接上限，形成背压（keepalive 连接池上限）
	MaxRPS          int // 全局限速（请求/秒），0 = 不限
	MemLimitMB      int // 软内存上限（debug.SetMemoryLimit），0 = 不限
	ReadTimeoutSec  int // 读完整请求（含 body）超时，防 slow-loris
	WriteTimeoutSec int // 写响应超时，防慢客户端占连接

	// 内存缓存（热门域名命中走本地，微秒级）
	CacheMaxTTL  int // 单条缓存最长保留秒数（同时是记录 TTL 的上限），0 = 关闭
	CacheEntries int // LRU 条目数上限

	// 上游熔断与故障转移
	CBThreshold int           // 连续失败多少次熔断
	CBCooldown  time.Duration // 熔断冷却时长
	Failover    []string      // 请求的上游熔断后，按此顺序尝试其它上游
}

// defaultMappings 与 Worker 版保持一致
var defaultMappings = map[string]Mapping{
	"/google":     {TargetDomain: "dns.google", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/cloudflare": {TargetDomain: "one.one.one.one", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/quad9":      {TargetDomain: "dns.quad9.net", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/adguard":    {TargetDomain: "dns.adguard-dns.com", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/dnspod":     {TargetDomain: "doh.pub", PathMapping: map[string]string{"/query-dns": "/dns-query", "/dns-query": "/dns-query"}},
	"/alidns":     {TargetDomain: "dns.alidns.com", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
}

// homepageHTML 未命中时返回的伪装 404 页面（与 Worker 版逐字一致；随系统深浅色自适应）
const homepageHTML = `<!DOCTYPE html>
<html lang="zh-CN" dir="ltr">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="color-scheme" content="light dark">
    <meta name="theme-color" content="#fff" media="(prefers-color-scheme: light)">
    <meta name="theme-color" content="#202124" media="(prefers-color-scheme: dark)">
    <title>404</title>
    <script>document.title = location.hostname || '404';</script>
    <style>
        html{
            font-size:125%;
            -webkit-text-size-adjust:100%;
        }
        body{
            --bg:#fff;
            --heading-color:#202124;
            --error-code-color:#5f6368;
            --link-color:#1a73e8;
            --button-bg:#1a73e8;
            --button-bg-active:#1967d2;
            --button-text:#fff;
            margin:8px;
            font-family:system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;
            font-size:75%;
            line-height:1.6em;
            color:#5f6368;
            background-color:var(--bg);
            word-wrap:break-word;
        }
        .interstitial{
            box-sizing:border-box;
            font-size:1em;
            line-height:1.6em;
            margin:14vh auto 0;
            max-width:600px;
            width:100%;
        }
        .icon{
            display:inline-block;
            width:72px;
            height:72px;
            margin:0 0 40px;
            background-image:url("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABIAQMAAABvIyEEAAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAENJREFUeF7tzbEJACEQRNGBLeAasBCza2lLEGx0CxFGG9hBMDDxRy/72O9FMnIFapGylsu1fgoBdkXfUHLrQgdfrlJN1BdYBjQQm3UAAAAASUVORK5CYII=");
            background-image:image-set(url("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABIAQMAAABvIyEEAAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAENJREFUeF7tzbEJACEQRNGBLeAasBCza2lLEGx0CxFGG9hBMDDxRy/72O9FMnIFapGylsu1fgoBdkXfUHLrQgdfrlJN1BdYBjQQm3UAAAAASUVORK5CYII=") 1x,url("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAJAAAACQAQMAAADdiHD7AAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAFJJREFUeF7t0cENgDAMQ9FwYgxG6WjpaIzCCAxQxVggFuDiCvlLOeRdHR9yzjncHVoq3npu+wQUrUuJHylSTmBaespJyJQoObUeyxDQb3bEm5Au81c0pSCD8HYAAAAASUVORK5CYII=") 2x);
            background-repeat:no-repeat;
            background-size:100%;
            -webkit-user-select:none;
            user-select:none;
        }
        h1{
            margin:0 0 16px;
            color:var(--heading-color);
            font-size:1.6em;
            font-weight:normal;
            line-height:1.25em;
            word-wrap:break-word;
        }
        h1 span{
            font-weight:500;
        }
        .message > p{
            display:inline;
            margin:15px 0;
        }
        .suggestions ul{
            list-style-type:none;
            margin-top:15px;
            padding-inline-start:0;
        }
        a{
            color:var(--link-color);
            text-decoration:none;
        }
        .error-code{
            margin-top:12px;
            color:var(--error-code-color);
            font-size:.8em;
            text-transform:uppercase;
        }
        .nav{
            margin-top:51px;
        }
        .nav::after{
            clear:both;
            content:'';
            display:table;
            width:100%;
        }
        .controls{
            float:left;
        }
        button{
            float:right;
            margin:0;
            padding:8px 16px;
            border:0;
            border-radius:20px;
            box-sizing:border-box;
            font-size:.875em;
            color:var(--button-text);
            background-color:var(--button-bg);
            cursor:pointer;
            -webkit-user-select:none;
            user-select:none;
        }
        button:active{
            background-color:var(--button-bg-active);
            outline:0;
        }
        @media (max-width:700px){
            .interstitial{
                padding:0 10%;
            }
        }
        @media (max-width:420px){
            .interstitial{
                padding:0 5%;
            }
            .nav{
                margin-top:30px;
            }
            .controls{
                float:none;
            }
            button{
                float:none;
                width:100%;
                margin:0;
                padding:16px 24px;
                font-size:.825em;
                font-weight:500;
            }
        }
        /* 手机与矮窗口：按钮固定在屏幕底部 */
        @media (min-width:240px) and (max-width:420px) and (min-height:401px),(min-width:421px) and (min-height:240px) and (max-height:560px){
            .interstitial{
                max-width:736px;
            }
            .main{
                padding-bottom:40px;
            }
            body .nav{
                position:fixed;
                left:0;
                right:0;
                bottom:0;
                z-index:2;
                width:100%;
                max-width:736px;
                margin:0 auto;
                padding-left:24px;
                padding-right:24px;
                background-color:var(--bg);
                box-shadow:0 -12px 24px var(--bg);
            }
        }
        @media (max-width:420px) and (orientation:portrait),(max-height:560px){
            body{
                margin:0 auto;
            }
            .interstitial{
                position:relative;
                box-sizing:border-box;
                margin:7vh auto 12px;
                padding:0 24px;
            }
            .interstitial p{
                font-size:.95em;
                line-height:1.61em;
                margin-top:8px;
            }
            .suggestions ul{
                margin-top:8px;
            }
            .icon{
                margin-bottom:5.69vh;
            }
            h1{
                font-size:1.5em;
                margin-bottom:8px;
            }
            .controls{
                margin:0;
            }
            .nav{
                box-sizing:border-box;
                width:100%;
                padding-bottom:8px;
            }
            button{
                font-family:Roboto-Regular,Helvetica;
                font-size:.933em;
                margin:6px 0;
                transform:translatez(0);
            }
        }
        @media (min-width:421px) and (min-height:500px) and (max-height:560px){
            .interstitial{
                margin-top:10vh;
            }
        }
        @media (min-height:400px) and (orientation:portrait){
            .interstitial{
                margin-bottom:145px;
            }
        }
        @media (min-height:299px){
            .nav{
                padding-bottom:16px;
            }
        }
        @media (min-height:500px) and (max-height:650px) and (max-width:414px) and (orientation:portrait){
            .interstitial{
                margin-top:7vh;
            }
        }
        @media (min-height:650px) and (max-width:414px) and (orientation:portrait){
            .interstitial{
                margin-top:10vh;
            }
        }
        /* 过小的屏幕（横条 / 小窗）：按钮不固定，跟在正文后面 */
        @media (max-height:400px) and (orientation:portrait),(max-height:239px) and (orientation:landscape),(max-width:419px) and (max-height:399px){
            .interstitial{
                display:flex;
                flex-direction:column;
                margin-bottom:0;
            }
            .main{
                flex:1 1 auto;
                order:0;
            }
            .nav{
                position:relative;
                flex:0 1 auto;
                order:1;
                width:100%;
                margin-top:8px;
                padding-left:0;
                padding-right:0;
            }
            button{
                padding:16px 24px;
            }
        }
        @media (max-width:239px) and (orientation:portrait){
            .nav{
                padding-left:0;
                padding-right:0;
            }
        }
        @media (max-width:640px),(max-height:640px){
            h1{
                margin:0 0 15px;
            }
        }
        @media (max-height:350px){
            h1{
                margin:0 0 15px;
            }
            .interstitial{
                margin-top:5%;
            }
            .nav{
                margin-top:30px;
            }
        }
        @media (min-width:420px) and (max-width:736px) and (min-height:240px) and (max-height:420px) and (orientation:landscape){
            .interstitial{
                margin-bottom:100px;
            }
        }
        @media (max-width:240px){
            .interstitial{
                padding:0 8px;
            }
            button{
                padding-left:12px;
                padding-right:12px;
            }
        }
        @media (max-width:120px){
            button{
                width:auto;
            }
        }
        /* 高对比度（Windows 强制色）模式会丢弃背景图：图标豁免，否则页面少一个元素 */
        @media (forced-colors:active){
            .icon{
                forced-color-adjust:none;
            }
        }
        @media (prefers-color-scheme:dark){
            body{
                --bg:#202124;
                --heading-color:#9aa0a6;
                --error-code-color:#9aa0a6;
                --link-color:#8ab4f8;
                --button-bg:#8ab4f8;
                --button-bg-active:#81a2d0;
                --button-text:#202124;
                color:#9aa0a6;
            }
            .icon{
                filter:invert(1);
            }
        }
    </style>
</head>
<body>
    <div class="interstitial">
        <div class="main">
            <!-- 图标取自 Chromium 网络错误页资源（BSD-3-Clause）；1x/2x 由 image-set 按像素密度选用 -->
            <div class="icon"></div>
            <div class="message">
                <h1><span id="heading">无法访问此网站</span></h1>
                <p id="typo">检查 <span id="host"></span> 中是否有拼写错误。</p>
                <div class="suggestions">
                    <ul>
                        <li><span id="s1">如果拼写无误，请</span><a href="javascript:void(0)" id="diagnose-link">尝试运行 Windows 网络诊断</a><span id="s2">。</span></li>
                    </ul>
                </div>
                <div class="error-code">DNS_PROBE_FINISHED_NXDOMAIN</div>
            </div>
        </div>
        <div class="nav">
            <div class="controls">
                <button id="reload-button">重新加载</button>
            </div>
        </div>
    </div>
    <script>
(function(){
/* 各界面语言的错误页文案（取自 Chrome 157 Windows 版实际渲染结果）。
   每条：[标题, 首段前, 首段后, 建议句前, 链接文字, 建议句后, 按钮文字, 字体栈编号] */
/* i18n:begin */
var S={
	"af":["Hierdie werf kan nie bereik word nie","Kyk of daar 'n tikfout is in ",".","As die spelling korrek is, kan jy ","Windows Network Diagnostics probeer gebruik",".","Herlaai",0],
	"am":["ይህ ጣቢያ ሊደረስበት አይችልም","በ"," ውስጥ የፊደል ግድፈት ካለ ይፈትሹ።","የፊደል አጻጻፉ ትክክል ከሆነ ","የWindows አውታረ መረብ ምርመራን ለማስኬድ ይሞክሩ","።","ዳግም ጫን",0],
	"ar":["لا يمكن الوصول إلى الموقع الإلكتروني هذا","تحقَّق ما إذا ورد خطأ إملائي في ",".","‏إذا تم التأكُد من عدم ورود أخطاء إملائية، يُرجى ","محاولة تشغيل بيانات التشخيص لشبكة Windows",".","إعادة التحميل",0],
	"bg":["Няма достъп до този сайт","Проверете дали "," е изписано правилно.","Ако изписването е правилно, ","стартирайте мрежова диагностика в Windows",".","Повторно зареждане",0],
	"bn":["এই সাইটটিতে পৌছানো যাচ্ছে না","","-এ কোনও বানান ভুল লেখা হয়েছে কিনা তা চেক করে দেখুন।","বানান সঠিক হলে, ","'Windows নেটওয়ার্ক ডায়াগনস্টিক' টুল চালিয়ে দেখুন","।","রিলোড করুন",6],
	"ca":["No es pot accedir a aquest lloc web","Comprova si "," està ben escrit.","Si està ben escrit, ","prova d'executar el diagnòstic de xarxes de Windows",".","Torna a carregar",0],
	"cs":["Tento web není dostupný","Zkontrolujte, zda v adrese "," nemáte překlep.","Pokud je adresa zadána správně, ","zkuste spustit Diagnostiku sítě systému Windows",".","Načíst znovu",0],
	"da":["Der kan ikke oprettes forbindelse til dette website","Tjek, om der er en stavefejl i ",".","Hvis stavningen er korrekt, kan du ","prøve at køre Windows Netværksdiagnosticering",".","Genindlæs",0],
	"de":["Die Website ist nicht erreichbar","Prüfe, ob „","“ einen Tippfehler enthält.","Wenn die URL keinen Tippfehler enthält, ","kannst du die Windows-Netzwerkdiagnose durchführen",".","Neu laden",0],
	"el":["Δεν είναι δυνατή η πρόσβαση σε αυτόν τον ιστότοπο","Ελέγξτε μήπως υπάρχει κάποιο ορθογραφικό λάθος στη διεύθυνση ",".","Εάν δεν υπάρχει ορθογραφικό λάθος, ","δοκιμάστε να εκτελέσετε έναν έλεγχο Διαγνωστικών δικτύου των Windows",".","Επαναφόρτωση",0],
	"en-GB":["This site can’t be reached","Check if there is a typo in ",".","If spelling is correct, ","try running windows network Diagnostics",".","Reload",0],
	"en-US":["This site can’t be reached","Check if there is a typo in ",".","If spelling is correct, ","try running Windows Network Diagnostics",".","Reload",0],
	"es-419":["No se puede acceder a este sitio","Revisa que no haya errores de ortografía en ",".","Si no hay errores, ","prueba ejecutar el diagnóstico de red de Windows",".","Volver a cargar",0],
	"es":["No se puede acceder a este sitio web","Comprueba si hay un error de escritura en ",".","Si está escrito correctamente, ","prueba a ejecutar el diagnóstico de red de Windows",".","Volver a cargar",0],
	"et":["Selle saidiga ei saa ühendust","Kontrollige, kas nimes "," on kirjaviga.","Kui õigekirjavigu pole, ","käitage Windowsi võrgudiagnostika",".","Laadi uuesti",0],
	"fa":["نمی‌توان به این سایت دست یافت","احتمال وجود غلط املائی در "," را بررسی کنید.","‏اگر املا صحیح است، ","«عیب‌یابی شبکه Windows» را اجرا کنید",".","مجدد بار کردن",0],
	"fi":["Sivustoon ei saada yhteyttä","Tarkista, sisältääkö "," kirjoitusvirheitä.","Jos kirjoitusasu on oikein, ","kokeile Windowsin verkon diagnostiikkaa",".","Lataa uudelleen",0],
	"fil":["Hindi makakonekta sa site na ito","Tingnan kung may typo sa ",".","Kung tama ang pagbabaybay, ","subukang patakbuhin ang Mga Diagnostic ng Network ng Windows",".","I-reload",0],
	"fr":["Ce site est inaccessible","Vérifiez si l'adresse "," est correcte.","S'il n'y a pas d'erreur, essayez de ","faire un diagnostic réseau Windows",".","Actualiser",0],
	"gu":["આ સાઇટ પર પહોંચી શકાતું નથી","ચેક કરો કે ","માં કોઈ ટાઇપિંગ ભૂલ તો નથી ને.","જો જોડણી સાચી હોય, તો ","Windows નેટવર્ક નિદાન ચલાવવાનો પ્રયાસ કરી જુઓ",".","ફરીથી લોડ કરો",0],
	"he":["לא ניתן לגשת לאתר הזה","יש לבדוק אם יש שגיאה בהקלדה ב-",".","‏אם האיות תקין, ","יש לנסות להפעיל את אבחון הרשת של Windows",".","טעינה מחדש",0],
	"hi":["इस साइट तक नहीं पहुंचा जा सकता","देखें कि "," की स्पेलिंग गलत तो नहीं है.","अगर स्पेलिंग सही है, तो ","Windows नेटवर्क डायग्नोस्टिक्स टूल चलाकर देखें",".","फिर लोड करें",0],
	"hr":["Web-lokacija ne može se dohvatiti","Provjerite sadrži li naziv "," pravopisnu pogrešku.","Ako nema pravopisnih pogrešaka, ","pokušajte pokrenuti Mrežnu dijagnostiku sustava Windows",".","Ponovno učitaj",0],
	"hu":["A webhely nem érhető el","Ellenőrizze, hogy van-e elírás a(z) "," domainnévben.","Ha helyesen írta, ","próbálkozzon a Windows hálózati diagnosztikájának futtatásával",".","Újratöltés",0],
	"id":["Situs ini tidak dapat dijangkau","Periksa apakah ada kesalahan ketik di ",".","Jika ejaan sudah benar, ","coba jalankan Diagnostik Jaringan Windows",".","Muat ulang",0],
	"it":["Impossibile raggiungere il sito","Controlla eventuali errori di battitura in ",".","Se l'ortografia è corretta, ","prova a eseguire lo strumento Diagnostica di rete Windows",".","Ricarica",0],
	"ja":["このサイトにアクセスできません",""," にタイプミスがないか確認してください。","タイプミスでない場合は、","Windows ネットワーク診断をお試しください","。","再読み込み",3],
	"kn":["ಈ ಸೈಟ್ ತಲುಪಲಾಗುವುದಿಲ್ಲ",""," ನ ಕಾಗುಣಿತ ತಪ್ಪಾಗಿದೆಯೇ ಎಂದು ಪರಿಶೀಲಿಸಿ.","ಕಾಗುಣಿತ ಸರಿಯಾಗಿದ್ದರೆ, ","Windows ನೆಟ್‌ವರ್ಕ್ ಡಯಾಗ್ನಾಸ್ಟಿಕ್ಸ್‌‌ ರನ್ ಮಾಡಿ ನೋಡಿ",".","ಮರುಲೋಡ್‌",0],
	"ko":["사이트에 연결할 수 없음","","에 오타가 있는지 확인하세요.","철자가 올바르다면 ","Windows 네트워크 진단을 실행","해 보세요.","새로고침",4],
	"lt":["Nepavyksta pasiekti šios svetainės","Patikrinkite, ar pavadinime "," nėra klaidos.","Jei parašyta tinkamai, ","pabandykite paleisti „Windows“ tinklo diagnostiką",".","Įkelti iš naujo",0],
	"lv":["Šī vietne nav sasniedzama","Pārbaudiet, vai adresē "," nav pareizrakstības kļūdas.","Ja nav pareizrakstības kļūdu, ","mēģiniet palaist Windows tīkla diagnostiku",".","Pārlādēt",0],
	"ml":["ഈ സൈറ്റ് ലഭ്യമാക്കാനാകുന്നില്ല",""," എന്നതിൽ അക്ഷരത്തെറ്റ് ഉണ്ടോ എന്ന് പരിശോധിക്കുക.","അക്ഷരങ്ങൾ ശരിയാണെങ്കിൽ, ","Windows നെറ്റ്‌വർക്ക് ഡയഗണോസ്‌റ്റിക്‌സ് റൺ ചെയ്‌തുനോക്കൂ",".","വീണ്ടും ലോഡ് ചെയ്യുക",7],
	"mr":["या साइटवर पोहचणे शक्य नाही",""," मध्ये टायपो आहे का ते तपासा.","शब्दलेखन योग्य असल्यास, ","Windows नेटवर्क निदान रन करून पहा",".","रीलोड करा",0],
	"ms":["Tapak ini tidak dapat dicapai","Semak jika terdapat kesilapan ejaan dalam ",".","Jika ejaan sudah betul, ","cuba jalankan Diagnostik Rangkaian Windows",".","Muat Semula",0],
	"nb":["Dette nettstedet er ikke tilgjengelig","Sjekk om det er en skrivefeil i ",".","Hvis stavemåten er korrekt, kan du ","prøve å kjøre Windows Nettverksdiagnose",".","Last inn på nytt",0],
	"nl":["Deze site is niet bereikbaar","Check of "," een typfout bevat.","Als de spelling klopt, ","voer je een Windows-netwerkcontrole uit",".","Opnieuw laden",0],
	"pl":["Ta witryna jest nieosiągalna","Sprawdź, czy w adresie "," nie ma błędu.","Jeśli pisownia jest poprawna, ","uruchom Diagnostykę sieci systemu Windows",".","Odśwież",0],
	"pt-BR":["Não é possível acessar esse site","Verifique se há um erro de digitação em ",".","Se o endereço estiver correto, ","tente executar o Diagnóstico de Rede do Windows",".","Recarregar",0],
	"pt-PT":["Não é possível aceder a este site","Verifique se existe algum erro ortográfico em ",".","Se a ortografia estiver correta, ","experimente executar o Diagnóstico de rede do Windows",".","Recarregar",0],
	"ro":["Acest site nu poate fi accesat","Verifică dacă există vreo greșeală de scriere în ",".","Dacă scrierea este corectă, ","încearcă să rulezi Diagnosticarea rețelei Windows",".","Reîncarcă",0],
	"ru":["Не удается получить доступ к сайту","Проверьте, нет ли опечаток в имени хоста ",".","Если все правильно, ",'воспользуйтесь инструментом "Диагностика сетей Windows"',".","Перезагрузить",0],
	"sk":["Tento web je nedostupný","Skontrolujte, či nie je v názve "," preklep.","Ak je pravopis správny, ","skúste spustiť nástroj Windows Network Diagnostics",".","Znova načítať",0],
	"sl":["Tega spletnega mesta ni mogoče doseči","Preverite, ali so v "," tipkarske napake.","Če je črkovanje pravilno, ","poskušajte zagnati orodje za omrežno diagnostiko sistema Windows",".","Znova naloži",0],
	"sr":["Овај сајт није доступан","Уверите се да "," не садржи словне грешке.","Ако нема грешака, ","покрените Windows дијагностику мреже",".","Учитај поново",0],
	"sv":["Webbplatsen kan inte nås","Kontrollera om det finns ett stavfel i ",".","Om stavningen är rätt ","testar du att köra nätverksdiagnostik för Windows",".","Läs in på nytt",0],
	"sw":["Imeshindwa kufungua tovuti hii","Angalia iwapo kuna kosa la hijai kwenye ",".","Iwapo hakuna kosa la hijai, ","jaribu kutekeleza Uchunguzi wa Mtandao",".","Pakia upya",0],
	"ta":["இந்தத் தளத்தை அணுக முடியவில்லை",""," என்பதில் எழுத்துப்பிழை உள்ளதா எனப் பார்க்கவும்.","எழுத்துப்பிழை இல்லையெனில் ","Windows நெட்வொர்க் சரிபார்ப்பை இயக்கிப் பார்க்கவும்",".","மீண்டும் ஏற்று",0],
	"te":["ఈ సైట్‌ను చేరుకోలేకపోయాము","","లో అక్షర దోషం ఉందేమో చెక్ చేయండి.","స్పెల్లింగ్ సరైనది అయితే, ","విండోల సమస్య విశ్లేషణలను రన్ చేయడానికి ట్రై చేయండి",".","మళ్లీ లోడ్ చేయి",0],
	"th":["ไม่สามารถเข้าถึงเว็บไซต์นี้","ตรวจสอบว่ามีการพิมพ์ผิดใน "," ไหม","หากการสะกดถูกต้อง ให้","ลองเรียกใช้การวินิจฉัยเครือข่ายของ Windows","","โหลดใหม่",5],
	"tr":["Bu siteye ulaşılamıyor",""," yazımında hata olup olmadığını kontrol edin.","Yazım doğruysa ","Windows Ağ Teşhisi'ni çalıştırmayı deneyin",".","Yeniden Yükle",0],
	"uk":["Немає зв’язку із сайтом","Перевірте, чи немає описки в імені хосту ",".","Якщо помилок немає, ","проведіть діагностику мережі Windows",".","Перезавантажити",0],
	"ur":["اس سائٹ تک رسائی حاصل نہیں کی جا سکتی ہے","چیک کریں کہ آیا "," میں کوئی ہجوں کی کوئی غلطی تو نہیں۔","‏اگر ہجے درست ہیں تو ","Windows نیٹ ورک کی تشخیصات چلانے کی کوشش کریں","۔","دوبارہ لوڈ کریں",0],
	"vi":["Không thể truy cập trang web này","Kiểm tra xem có lỗi chính tả trong "," hay không.","Nếu đúng chính tả, hãy ","thử chạy Chẩn đoán mạng của Windows",".","Tải lại",0],
	"zh-CN":["无法访问此网站","检查 "," 中是否有拼写错误。","如果拼写无误，请","尝试运行 Windows 网络诊断","。","重新加载",1],
	"zh-TW":["無法連上這個網站","請檢查 "," 中是否有錯字。","如果拼字正確，","請嘗試執行 Windows 網路診斷","。","重新載入",2]
};
var F=[
	"Segoe UI|Tahoma|sans-serif",
	"Segoe UI|Arial|Microsoft Yahei|sans-serif",
	"Segoe UI|Arial|Microsoft Jhenghei|sans-serif",
	"Segoe UI|Arial|Meiryo|sans-serif",
	"Segoe UI|Arial|Malgun Gothic|Gulim|sans-serif",
	"Tahoma|sans-serif",
	"Vrinda",
	"Segoe UI|Arial|AnjaliOldLipi|Rachana|Kartika"
];
var R={"ar":1,"fa":1,"he":1,"ur":1};
/* i18n:end */
var el=function(id){return document.getElementById(id)};
var host=location.hostname||'404';
el('host').textContent=host;
var t=String(navigator.language||'').toLowerCase();
// 表里的键是 Chrome 的规范写法（en-GB / pt-PT / zh-CN），而 t 已转成小写：另建一张小写键的索引
var L={};
for(var k in S)L[k.toLowerCase()]=k;
var pick=function(t){
    if(L[t])return L[t];
    var p=t.split('-')[0];
    if(p==='zh')return /(?:^|-)(tw|hk|mo|hant)(?:-|$)/.test(t)?'zh-TW':'zh-CN';
    if(L[p])return L[p];
    return p==='pt'?'pt-BR':'en-US';
};
var tag=t?pick(t):'zh-CN';
var s=S[tag];
el('heading').textContent=s[0];
el('typo').textContent=s[1]+host+s[2];
el('s1').textContent=s[3];
el('diagnose-link').textContent=s[4];
el('s2').textContent=s[5];
el('reload-button').textContent=s[6];
document.documentElement.lang=tag.split('-')[0];
document.documentElement.dir=R[tag]?'rtl':'ltr';
// Windows 上按语言换用 Chrome 的字体栈，其余平台沿用系统字体（与 Chrome 一致）。
// F 用 | 分隔字体名，带空格的名字加双引号
if(/win/i.test((navigator.userAgentData&&navigator.userAgentData.platform)||navigator.platform||'')){
    document.body.style.fontFamily=F[s[7]].split('|').map(function(f){return f.indexOf(' ')>=0?'"'+f+'"':f}).join(', ');
}
el('reload-button').onclick=function(){location.reload()};
})();
    </script>
</body>
</html>`

// loadConfig 从环境变量读取配置
func loadConfig() Config {
	cfg := Config{
		Mappings:        defaultMappings,
		DefaultUpstream: "cloudflare",
		MaxInFlight:     envInt("MAX_INFLIGHT", 512),
		MaxConnsPerHost: envInt("MAX_CONNS_PER_HOST", 64),
		MaxRPS:          envInt("MAX_RPS", 0),
		MemLimitMB:      envInt("MEM_LIMIT_MB", 512),
		ReadTimeoutSec:  envInt("READ_TIMEOUT_SEC", 20),
		WriteTimeoutSec: envInt("WRITE_TIMEOUT_SEC", 60),
		CacheMaxTTL:     envInt("CACHE_MAX_TTL", 300),
		CacheEntries:    envInt("CACHE_ENTRIES", 10000),
		CBThreshold:     envInt("CB_THRESHOLD", 5),
		CBCooldown:      time.Duration(envInt("CB_COOLDOWN_SEC", 30)) * time.Second,
	}

	if raw := strings.TrimSpace(os.Getenv("DOMAIN_MAPPINGS")); raw != "" {
		var parsed map[string]Mapping
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			log.Printf("DOMAIN_MAPPINGS 解析失败，回退默认映射: %v", err)
		} else if len(parsed) > 0 {
			cfg.Mappings = parsed
		}
	}

	// 故障转移链：默认 cloudflare → google → adguard（全部海外可达且无过滤差异主嫌时可用）。
	// 必须在 DOMAIN_MAPPINGS 覆盖之后再校验，否则自定义映射里的上游名会被误判不在映射表而丢弃
	chain := strings.TrimSpace(os.Getenv("FAILOVER_CHAIN"))
	if chain == "" {
		chain = "cloudflare,google,adguard"
	}
	for _, name := range strings.Split(chain, ",") {
		if name = strings.Trim(strings.ToLower(name), "/ "); name != "" {
			if m, ok := cfg.Mappings["/"+name]; ok && m.TargetDomain != "" {
				cfg.Failover = append(cfg.Failover, name)
			} else {
				log.Printf("FAILOVER_CHAIN 里的 %q 不在映射表中，忽略", name)
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("ALLOWED_HOSTS")); raw != "" {
		for _, h := range strings.Split(raw, ",") {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				cfg.AllowedHosts = append(cfg.AllowedHosts, h)
			}
		}
	}

	if v := strings.TrimSpace(os.Getenv("DEFAULT_UPSTREAM")); v != "" {
		cfg.DefaultUpstream = strings.TrimPrefix(strings.ToLower(v), "/")
	}

	if v := strings.TrimSpace(os.Getenv("CACHE_TTL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.CacheTTL = n
		}
	}

	cfg.Verbose = os.Getenv("VERBOSE") == "1"
	cfg.HealthPath = strings.TrimSpace(os.Getenv("HEALTH_PATH"))
	cfg.MetricsPath = strings.TrimSpace(os.Getenv("METRICS_PATH"))
	return cfg
}

// envInt 读取整数环境变量，非法或缺省时用默认值；<=0 时按调用方语义处理
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("%s=%q 非法，使用默认值 %d", key, v, def)
		return def
	}
	return n
}

// hostAllowed 判断 Host 是否在白名单内
func (c Config) hostAllowed(host string) bool {
	if len(c.AllowedHosts) == 0 {
		return true
	}
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, allowed := range c.AllowedHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

// providerTarget 构造指定上游的 DoH 目标 URL（裸路径与故障转移共用）
func (c Config) providerTarget(provider, rawQuery string) (string, bool) {
	m, ok := c.Mappings["/"+strings.TrimPrefix(provider, "/")]
	if !ok || m.TargetDomain == "" {
		return "", false
	}
	u := "https://" + m.TargetDomain + "/dns-query"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u, true
}

// resolveTarget 返回（上游名, 转发目标 URL）；ok=false 表示不转发
func (c Config) resolveTarget(path, rawQuery string) (string, string, bool) {
	// 最长前缀优先
	var prefix string
	for p := range c.Mappings {
		if strings.HasPrefix(path, p) && len(p) > len(prefix) {
			prefix = p
		}
	}

	if prefix == "" {
		// 兼容 https://域名/dns-query 这种「无前缀」的标准 DoH 写法
		bare := strings.TrimRight(path, "/")
		if bare == "/dns-query" || bare == "/query-dns" {
			if u, ok := c.providerTarget(c.DefaultUpstream, rawQuery); ok {
				return c.DefaultUpstream, u, true
			}
		}
		return "", "", false
	}

	m := c.Mappings[prefix]
	if m.TargetDomain == "" {
		return "", "", false
	}

	rest := path[len(prefix):]
	// 按路径映射表重写（键按长度倒序，保证最长匹配）
	keys := make([]string, 0, len(m.PathMapping))
	for k := range m.PathMapping {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	targetPath := rest
	for _, k := range keys {
		if strings.HasPrefix(rest, k) {
			targetPath = m.PathMapping[k] + rest[len(k):]
			break
		}
	}
	if targetPath == "" {
		targetPath = "/"
	}
	if !strings.HasPrefix(targetPath, "/") {
		targetPath = "/" + targetPath
	}

	u := "https://" + m.TargetDomain + targetPath
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return strings.TrimPrefix(prefix, "/"), u, true
}

// isDoHRequest 判断请求是否确实是 DoH 协议流量
func isDoHRequest(r *http.Request) bool {
	a := strings.ToLower(r.Header.Get("Accept"))
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(a, "application/dns-message") || strings.Contains(ct, "application/dns-message")
}

// ---------- DNS 报文解析（用于缓存键与 TTL 处理） ----------

// parseDNSQuery 从查询报文提取（事务 ID, 缓存键）。
// 缓存键 = flags + question 段（QNAME+QTYPE+QCLASS），不含事务 ID——
// 不同客户端的 ID 不同但应答相同，命中后按客户端 ID 改写即可。
func parseDNSQuery(body []byte) (uint16, []byte, bool) {
	if len(body) < 17 { // 12 头 + 最短 question(1+4)
		return 0, nil, false
	}
	if int(body[4])<<8|int(body[5]) != 1 { // QDCOUNT 必须为 1
		return 0, nil, false
	}
	id := uint16(body[0])<<8 | uint16(body[1])
	i := 12
	for i < len(body) {
		l := int(body[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 { // question 里不该有压缩指针
			return 0, nil, false
		}
		i += 1 + l
	}
	if i+4 > len(body) {
		return 0, nil, false
	}
	key := make([]byte, 0, 2+(i+4-12))
	key = append(key, body[2], body[3]) // flags
	key = append(key, body[12:i+4]...)  // question + qtype + qclass
	return id, key, true
}

// rrNext 从 i 处解析一条 RR，返回（TTL 字段偏移, 记录类型, 下一记录偏移）
func rrNext(b []byte, i int) (ttlOff, typ, next int, ok bool) {
	if i >= len(b) {
		return 0, 0, 0, false
	}
	if b[i]&0xC0 == 0xC0 { // 压缩指针名
		i += 2
	} else {
		for i < len(b) {
			l := int(b[i])
			if l == 0 {
				i++
				break
			}
			if l&0xC0 != 0 {
				return 0, 0, 0, false
			}
			i += 1 + l
		}
	}
	if i+10 > len(b) {
		return 0, 0, 0, false
	}
	typ = (int(b[i]) << 8) | int(b[i+1])
	rdlen := (int(b[i+8]) << 8) | int(b[i+9])
	return i + 4, typ, i + 10 + rdlen, true
}

// isPseudoRR EDNS0 OPT / TSIG / TKEY 不是真实资源记录，其 TTL 字段含义不同（OPT 恒为 0），
// 参与 TTL 计算会把整条应答误判成不可缓存，必须跳过
func isPseudoRR(typ int) bool {
	return typ == 41 /* OPT */ || typ == 249 /* TKEY */ || typ == 250 /* TSIG */
}

// dnsSectionStart 返回应答记录区起始位置与 AN+NS+AR 记录总数
func dnsSectionStart(b []byte) (int, int, bool) {
	if len(b) < 12 {
		return 0, 0, false
	}
	qd := (int(b[4]) << 8) | int(b[5])
	i := 12
	for q := 0; q < qd; q++ {
		for i < len(b) {
			l := int(b[i])
			if l == 0 {
				i++
				break
			}
			if l&0xC0 != 0 { // 指针只可能出现在记录区，question 里遇到就按 2 字节跳过
				i += 2
				break
			}
			i += 1 + l
		}
		i += 4 // QTYPE + QCLASS
	}
	if i > len(b) {
		return 0, 0, false
	}
	an := (int(b[6]) << 8) | int(b[7])
	ns := (int(b[8]) << 8) | int(b[9])
	ar := (int(b[10]) << 8) | int(b[11])
	return i, an + ns + ar, true
}

// dnsMinTTL 返回应答里最小的记录 TTL；解析失败返回 -1
func dnsMinTTL(b []byte) int {
	start, count, ok := dnsSectionStart(b)
	if !ok {
		return -1
	}
	min := -1
	i := start
	for c := 0; c < count; c++ {
		ttlOff, typ, next, ok2 := rrNext(b, i)
		if !ok2 || ttlOff+4 > len(b) {
			return -1
		}
		if !isPseudoRR(typ) {
			ttl := int(b[ttlOff])<<24 | int(b[ttlOff+1])<<16 | int(b[ttlOff+2])<<8 | int(b[ttlOff+3])
			if min < 0 || ttl < min {
				min = ttl
			}
		}
		i = next
	}
	return min
}

// decrementTTLs 把应答中所有记录 TTL 减去 sec（缓存命中时使用），返回新切片；
// 解析失败时原样返回，宁可 TTL 偏大也不破坏报文
func decrementTTLs(b []byte, sec int) []byte {
	out := append([]byte(nil), b...)
	start, count, ok := dnsSectionStart(out)
	if !ok {
		return b
	}
	i := start
	for c := 0; c < count; c++ {
		ttlOff, typ, next, ok2 := rrNext(out, i)
		if !ok2 || ttlOff+4 > len(out) {
			return b
		}
		if !isPseudoRR(typ) { // OPT 的 TTL 字段是扩展 RCODE/标志位，绝不能递减
			ttl := int(out[ttlOff])<<24 | int(out[ttlOff+1])<<16 | int(out[ttlOff+2])<<8 | int(out[ttlOff+3])
			if ttl > sec {
				ttl -= sec
			} else {
				ttl = 0
			}
			out[ttlOff], out[ttlOff+1], out[ttlOff+2], out[ttlOff+3] = byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl)
		}
		i = next
	}
	return out
}

// rewriteID 把应答的事务 ID 改成客户端的（缓存命中时使用）
func rewriteID(b []byte, id uint16) []byte {
	out := append([]byte(nil), b...)
	out[0] = byte(id >> 8)
	out[1] = byte(id)
	return out
}

// ---------- LRU 应答缓存 ----------

type cacheEntry struct {
	key         string
	body        []byte
	contentType string
	fetchedAt   time.Time
	ttl         int // 有效秒数 = min(记录最小 TTL, CacheMaxTTL)
}

type dnsCache struct {
	mu      sync.Mutex
	max     int
	ll      *list.List
	entries map[string]*list.Element
	hits    atomic.Uint64
	misses  atomic.Uint64
}

func newDNSCache(maxEntries int) *dnsCache {
	if maxEntries <= 0 {
		return nil
	}
	return &dnsCache{
		max:     maxEntries,
		ll:      list.New(),
		entries: make(map[string]*list.Element, maxEntries/4+8),
	}
}

func (c *dnsCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	el, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	e := el.Value.(cacheEntry)
	c.ll.MoveToFront(el)
	c.mu.Unlock()
	if time.Since(e.fetchedAt) >= time.Duration(e.ttl)*time.Second {
		c.evict(key)
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	c.hits.Add(1)
	return e, true
}

func (c *dnsCache) put(key string, e cacheEntry) {
	e.key = key
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value = e
		c.ll.MoveToFront(el)
		return
	}
	c.entries[key] = c.ll.PushFront(e)
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		delete(c.entries, back.Value.(cacheEntry).key)
		c.ll.Remove(back)
	}
}

func (c *dnsCache) evict(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.ll.Remove(el)
		delete(c.entries, key)
	}
}

func (c *dnsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// ---------- 上游熔断器 ----------

type providerHealth struct {
	mu        sync.Mutex
	fails     int
	until     time.Time
	threshold int
	cooldown  time.Duration
}

// allow 是否允许尝试该上游（熔断冷却结束后放行一次探测请求）
func (h *providerHealth) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.threshold <= 0 || h.fails < h.threshold {
		return true
	}
	return time.Now().After(h.until)
}

func (h *providerHealth) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.fails = 0
		return
	}
	h.fails++
	if h.fails >= h.threshold {
		h.until = time.Now().Add(h.cooldown)
	}
}

func (h *providerHealth) snapshot() (healthy bool, fails int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fails < h.threshold, h.fails
}

// cacheControl 计算响应缓存策略；第二个返回值表示是否需要 Vary: Accept
func (c Config) cacheControl(r *http.Request) (string, bool) {
	if c.CacheTTL > 0 && r.Method == http.MethodGet &&
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/dns-message") {
		return fmt.Sprintf("public, max-age=%d", c.CacheTTL), true
	}
	return "no-store", false
}

// hop-by-hop 头，转发时需要剥掉
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type server struct {
	cfg    Config
	client *http.Client

	// 并发控制与运行指标
	inFlight    chan struct{}
	inFlightNum atomic.Int64
	totalReq    atomic.Uint64
	totalErr    atomic.Uint64
	startedAt   time.Time

	rpsMu       sync.Mutex
	windowStart time.Time
	windowCount int

	// 应答缓存与上游熔断
	cache    *dnsCache
	healthMu sync.Mutex
	health   map[string]*providerHealth
}

func newServer(cfg Config) *server {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 512
	}
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = 64
	}
	if cfg.CBThreshold <= 0 {
		cfg.CBThreshold = 5
	}
	if cfg.CBCooldown <= 0 {
		cfg.CBCooldown = 30 * time.Second
	}

	// 传输层按 nginx 的连接复用思路调优：
	//   MaxConnsPerHost 形成背压（多余的请求排队等空闲连接，而不是无限新建）
	//   ClientSessionCache 复用 TLS 会话，省掉重复握手（高并发下 CPU 的大头）
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxConnsPerHost * 4,
		MaxIdleConnsPerHost:   cfg.MaxConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}

	s := &server{
		cfg:       cfg,
		client:    &http.Client{Transport: transport, Timeout: 15 * time.Second},
		inFlight:  make(chan struct{}, cfg.MaxInFlight),
		startedAt: time.Now(),
		health:    make(map[string]*providerHealth),
	}
	if cfg.CacheMaxTTL > 0 {
		s.cache = newDNSCache(cfg.CacheEntries)
	}
	for name := range cfg.Mappings {
		s.health[strings.TrimPrefix(name, "/")] = &providerHealth{
			threshold: cfg.CBThreshold,
			cooldown:  cfg.CBCooldown,
		}
	}
	return s
}

// healthFor 取上游健康状态（未知上游按需创建）
func (s *server) healthFor(name string) *providerHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h, ok := s.health[name]
	if !ok {
		h = &providerHealth{threshold: s.cfg.CBThreshold, cooldown: s.cfg.CBCooldown}
		s.health[name] = h
	}
	return h
}

// candidates 返回本次请求可尝试的上游顺序：首选 + 健康检查通过的转移链
func (s *server) candidates(primary string) []string {
	out := make([]string, 0, 1+len(s.cfg.Failover))
	if s.healthFor(primary).allow() {
		out = append(out, primary)
	}
	for _, f := range s.cfg.Failover {
		if f == primary {
			continue
		}
		if s.healthFor(f).allow() {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		// 全部处于熔断冷却中：仍然尝试首选，避免把所有请求直接判死
		out = append(out, primary)
	}
	return out
}

// tooManyRequests 全局限速（按秒窗口计数）
func (s *server) tooManyRequests() bool {
	if s.cfg.MaxRPS <= 0 {
		return false
	}
	s.rpsMu.Lock()
	defer s.rpsMu.Unlock()
	now := time.Now()
	if now.Sub(s.windowStart) >= time.Second {
		s.windowStart = now
		s.windowCount = 0
	}
	s.windowCount++
	return s.windowCount > s.cfg.MaxRPS
}

func (s *server) writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	providers := make(map[string]any, len(s.health))
	s.healthMu.Lock()
	for name, h := range s.health {
		healthy, fails := h.snapshot()
		providers[name] = map[string]any{"healthy": healthy, "consecutive_failures": fails}
	}
	s.healthMu.Unlock()

	m := map[string]any{
		"in_flight":       s.inFlightNum.Load(),
		"total_requests":  s.totalReq.Load(),
		"upstream_errors": s.totalErr.Load(),
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		"max_in_flight":   s.cfg.MaxInFlight,
		"max_rps":         s.cfg.MaxRPS,
		"providers":       providers,
	}
	if s.cache != nil {
		m["cache_hits"] = s.cache.hits.Load()
		m["cache_misses"] = s.cache.misses.Load()
		m["cache_entries"] = s.cache.len()
		m["cache_max_ttl"] = s.cfg.CacheMaxTTL
	} else {
		m["cache_entries"] = 0
	}
	_ = json.NewEncoder(w).Encode(m)
}

func (s *server) camo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, homepageHTML)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	// 任何 panic 都不能带垮连接之外的状态
	defer func() {
		if err := recover(); err != nil {
			log.Printf("panic: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()

	// 在途请求上限：满则快速失败，避免 goroutine 无界堆积
	select {
	case s.inFlight <- struct{}{}:
		s.inFlightNum.Add(1)
		s.totalReq.Add(1)
		defer func() {
			<-s.inFlight
			s.inFlightNum.Add(-1)
		}()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}

	if s.tooManyRequests() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	if !s.cfg.hostAllowed(r.Host) {
		s.camo(w)
		return
	}

	if s.cfg.HealthPath != "" && r.URL.Path == s.cfg.HealthPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
		return
	}

	if s.cfg.MetricsPath != "" && r.URL.Path == s.cfg.MetricsPath {
		s.writeMetrics(w)
		return
	}

	provider, target, ok := s.cfg.resolveTarget(r.URL.Path, r.URL.RawQuery)
	if !ok && (r.URL.Path == "/" || r.URL.Path == "") && isDoHRequest(r) {
		provider, target, ok = s.cfg.resolveTarget("/dns-query", r.URL.RawQuery)
	}
	if !ok {
		s.camo(w)
		return
	}

	// 读请求体（DoH 报文很小，64KB 上限；超限直接拒绝，
	// 避免把静默截断后的畸形报文发给上游浪费一次查询）
	var bodyBuf []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if len(buf) > maxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		bodyBuf = buf
	}

	// 组装缓存键（只对 wire 格式 DoH 查询缓存，JSON 查询不缓存）
	cacheKey := ""
	var reqID uint16
	if s.cache != nil {
		if id, qkey, ok := dohQueryOf(r, bodyBuf); ok {
			reqID = id
			cacheKey = provider + "|" + string(qkey)
		}
	}

	// 命中内存缓存：改写 ID、递减 TTL 后直接返回
	if cacheKey != "" {
		if e, hit := s.cache.get(cacheKey); hit {
			elapsed := int(time.Since(e.fetchedAt).Seconds())
			out := rewriteID(decrementTTLs(e.body, elapsed), reqID)
			s.writeViaHeaders(w, r, e.contentType)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			if s.cfg.Verbose {
				log.Printf("%s %s%s -> cache HIT (%dB, %s)", r.Method, r.Host, r.URL.Path, len(out), time.Since(started).Round(time.Millisecond))
			}
			return
		}
	}

	// 依次尝试：首选上游 → 故障转移链（熔断中的上游会被跳过）
	var (
		resp      *http.Response
		usedURL   = target
		lastErr   error
		gotStatus int
		cacheable []byte
		readBuf   []byte
	)
	for _, cand := range s.candidates(provider) {
		attemptURL := target
		if cand != provider {
			u, ok2 := s.cfg.providerTarget(cand, r.URL.RawQuery)
			if !ok2 {
				continue
			}
			attemptURL = u
		}

		var body io.Reader
		if bodyBuf != nil {
			body = bytes.NewReader(bodyBuf)
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, attemptURL, body)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header = r.Header.Clone()
		for _, h := range hopHeaders {
			req.Header.Del(h)
		}
		req.Header.Del("Host")
		req.Header.Del("Content-Length")
		// Accept-Encoding 也不能透传：transport 只对「自己加的」gzip 做透明解压，
		// 克隆客户端的会导致压缩应答原样进缓存，未带该头的客户端命中后拿到压缩体无法解析。
		// 剥掉后由 transport 自行协商，缓存恒存 identity 字节（与边缘版语义一致）。
		req.Header.Del("Accept-Encoding")
		// 始终显式设置长度，避免 chunked 转发被部分 DoH 上游拒绝
		req.ContentLength = int64(len(bodyBuf))

		resp, err = s.client.Do(req)
		if err != nil {
			s.healthFor(cand).record(false)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// 上游 5xx 视为不可用，读完响应体以复用连接后转移
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
			s.healthFor(cand).record(false)
			lastErr = fmt.Errorf("%s 返回 %d", cand, resp.StatusCode)
			gotStatus = resp.StatusCode
			resp = nil
			continue
		}
		s.healthFor(cand).record(true)

		// 可缓存响应先整段读出（DoH 应答极小），顺便落缓存。
		// 多读 1 字节用于检测截断：超出上限时不能只写截断体——客户端会按上游
		// Content-Length 挂起等剩余字节——改为把已读部分连同剩余一起写完。
		// 读体失败（上游中途断流）记该上游失败并继续候选链（对齐边缘版行为）
		if cacheKey != "" && resp.StatusCode == http.StatusOK {
			buf, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
			if readErr != nil {
				resp.Body.Close()
				s.healthFor(cand).record(false)
				lastErr = readErr
				resp = nil
				continue
			}
			readBuf = buf
			if len(readBuf) <= maxBodyBytes {
				cacheable = readBuf
				s.storeCache(cacheKey, cacheable, resp.Header.Get("Content-Type"))
			}
		}
		usedURL = attemptURL
		break
	}

	if resp == nil {
		s.totalErr.Add(1)
		if s.cfg.Verbose {
			log.Printf("upstream failed: %s %s -> %v", r.Method, target, lastErr)
		}
		w.Header().Set("Retry-After", "2")
		code := http.StatusBadGateway
		if gotStatus != 0 {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, "upstream error", code)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	cc, needVary := s.cfg.cacheControl(r)
	w.Header().Set("Cache-Control", cc)
	if needVary {
		if v := w.Header().Get("Vary"); v != "" {
			w.Header().Set("Vary", v+", Accept")
		} else {
			w.Header().Set("Vary", "Accept")
		}
	}
	if cacheKey != "" {
		w.Header().Set("X-Cache", "MISS")
	}

	w.WriteHeader(resp.StatusCode)
	var n int64
	if cacheable != nil {
		m, _ := w.Write(cacheable)
		n = int64(m)
	} else {
		if readBuf != nil {
			// 截断检测已消费了前 maxBodyBytes+1 字节，先补写已读部分再续写剩余
			m, _ := w.Write(readBuf)
			n = int64(m)
		}
		m, _ := io.Copy(w, resp.Body)
		n += m
	}

	if s.cfg.Verbose {
		log.Printf("%s %s%s -> %s %d (%dB, %s)", r.Method, r.Host, r.URL.Path, usedURL, resp.StatusCode, n, time.Since(started).Round(time.Millisecond))
	}
}

// writeViaHeaders 设置缓存命中响应的基础响应头
func (s *server) writeViaHeaders(w http.ResponseWriter, r *http.Request, contentType string) {
	h := w.Header()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	} else {
		h.Set("Content-Type", "application/dns-message")
	}
	cc, needVary := s.cfg.cacheControl(r)
	h.Set("Cache-Control", cc)
	if needVary {
		if v := h.Get("Vary"); v != "" {
			h.Set("Vary", v+", Accept")
		} else {
			h.Set("Vary", "Accept")
		}
	}
}

// storeCache 判定并写入缓存：仅缓存 rcode=0/3 且有有效 TTL 的应答
func (s *server) storeCache(key string, body []byte, contentType string) {
	if s.cache == nil || len(body) < 12 || s.cfg.CacheMaxTTL <= 0 {
		return
	}
	rcode := int(body[3] & 0x0F)
	if rcode != 0 && rcode != 3 {
		return
	}
	ttl := dnsMinTTL(body)
	if ttl <= 0 {
		return
	}
	if ttl > s.cfg.CacheMaxTTL {
		ttl = s.cfg.CacheMaxTTL
	}
	if contentType == "" {
		contentType = "application/dns-message"
	}
	s.cache.put(key, cacheEntry{
		body:        append([]byte(nil), body...),
		contentType: contentType,
		fetchedAt:   time.Now(),
		ttl:         ttl,
	})
}

// dohQueryOf 提取（事务 ID, 缓存键）：POST 报文优先，其次 GET 的 ?dns= 参数
func dohQueryOf(r *http.Request, body []byte) (uint16, []byte, bool) {
	if len(body) > 0 {
		if id, k, ok := parseDNSQuery(body); ok {
			return id, k, true
		}
		return 0, nil, false
	}
	if v := r.URL.Query().Get("dns"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			raw, err = base64.URLEncoding.DecodeString(v)
			if err != nil {
				return 0, nil, false
			}
		}
		return parseDNSQuery(raw)
	}
	return 0, nil, false
}

func main() {
	addr := flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "监听地址，如 :8080")
	tlsAddr := flag.String("tls-listen", os.Getenv("TLS_LISTEN"), "额外的 TLS 监听地址，如 :8444（留空则把 -listen 变成 HTTPS）")
	certFile := flag.String("tls-cert", os.Getenv("TLS_CERT"), "TLS 证书路径（可选）")
	keyFile := flag.String("tls-key", os.Getenv("TLS_KEY"), "TLS 私钥路径（可选）")
	flag.Parse()

	cfg := loadConfig()
	if cfg.MemLimitMB > 0 {
		// 软内存上限：到顶先触发 GC 降速，而不是把整机拖进 OOM
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	srv := newServer(cfg)

	hasTLS := *certFile != "" && *keyFile != ""
	plainAddr, secureAddr := *addr, ""
	switch {
	case hasTLS && *tlsAddr != "":
		secureAddr = *tlsAddr // 双监听：HTTP + HTTPS
	case hasTLS:
		plainAddr, secureAddr = "", *addr // 单监听：-listen 直接变 HTTPS
	}

	newHTTPServer := func(a string) *http.Server {
		return &http.Server{
			Addr:    a,
			Handler: srv,
			// 超时族（对应 nginx client_header_timeout / client_body_timeout / send_timeout）
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Duration(cfg.ReadTimeoutSec) * time.Second,
			WriteTimeout:      time.Duration(cfg.WriteTimeoutSec) * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    16 << 10, // DoH 头很小，限制头部大小防慢速大包
		}
	}

	servers := make([]*http.Server, 0, 2)
	errCh := make(chan error, 2)

	if plainAddr != "" {
		s := newHTTPServer(plainAddr)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTP  监听 %s", plainAddr)
			errCh <- s.ListenAndServe()
		}()
	}
	if secureAddr != "" {
		s := newHTTPServer(secureAddr)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTPS 监听 %s（cert=%s）", secureAddr, *certFile)
			errCh <- s.ListenAndServeTLS(*certFile, *keyFile)
		}()
	}
	if len(servers) == 0 {
		log.Fatalf("没有可用的监听地址")
	}

	log.Printf("doh-proxy 启动：upstreams=%d allowed_hosts=%d default=%s cache_ttl=%d",
		len(cfg.Mappings), len(cfg.AllowedHosts), cfg.DefaultUpstream, cfg.CacheTTL)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务退出: %v", err)
		}
	case sig := <-stop:
		log.Printf("收到信号 %s，开始优雅退出", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			if err := s.Shutdown(ctx); err != nil {
				log.Printf("关闭 %s 超时，强制断开剩余连接: %v", s.Addr, err)
				_ = s.Close()
			}
		}
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
