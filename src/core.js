/**
 * 平台无关的核心逻辑：路径映射解析 + DoH 反向代理
 *                + 应答缓存（LRU） + 上游熔断/故障转移 + 健康检查/指标
 *
 * 该模块同时被 Cloudflare Worker（_worker.js）与
 * 腾讯 EdgeOne Pages 边缘函数（middleware.js）复用，
 * 因此只能使用 Web 标准 API（fetch / Request / Response / URL / Headers / Uint8Array），
 * 不得引入任何 Node.js 内置模块或第三方依赖。
 *
 * 缓存键/报文改写/熔断语义与 Go 版（server/main.go）逐条对齐：
 *   - 缓存键 = 上游名 + DNS 头 flags + question 段（QNAME/QTYPE/QCLASS），事务 ID 不参与
 *   - 命中时先按存活秒数递减记录 TTL（跳过 OPT/TKEY/TSIG），再改写应答 ID 为客户端 ID
 *   - 只缓存 rcode 0/3、最小记录 TTL>0（rcode=3 无 SOA 自然不缓存）的 200 应答
 *   - 熔断：连续失败 CB_THRESHOLD 次 OPEN，冷却 CB_COOLDOWN_SEC 后放行；
 *     全部 OPEN 时仍打首选上游兜底；4xx 不算失败，原样透传
 *
 * 平台差异（与 Go 版的已知不同，勿混淆）：
 *   - 缓存/熔断/计数状态都是 **isolate 级**（每个边缘节点、每次部署/闲置回收后重置），
 *     属 best-effort，不承诺 Go 进程级的稳定性
 *   - 无 MAX_INFLIGHT / MAX_RPS / MEM_LIMIT：边缘运行时的并发与内存由平台治理，
 *     用户态信号量在同一 isolate 内意义有限
 *   - 环境变量 CACHE_MAX_TTL / CACHE_ENTRIES / CB_THRESHOLD / CB_COOLDOWN_SEC /
 *     FAILOVER_CHAIN 在 isolate 首个请求时读取一次
 */

/** 默认路径映射（可按需增删） */
export const DEFAULT_PATH_MAPPINGS = {
	'/google': {
		targetDomain: 'dns.google',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/cloudflare': {
		targetDomain: 'one.one.one.one',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/quad9': {
		targetDomain: 'dns.quad9.net',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/adguard': {
		targetDomain: 'dns.adguard-dns.com',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	// 国内可达的 DoH（部署在国内边缘节点时优先使用）
	'/dnspod': {
		targetDomain: 'doh.pub',
		pathMapping: {
			'/query-dns': '/dns-query',
			'/dns-query': '/dns-query',
		},
	},
	'/alidns': {
		targetDomain: 'dns.alidns.com',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	// 在此继续添加映射
};

/** 未命中映射时返回的伪装 404 页面（刻意伪装成浏览器断网页；随系统深浅色自适应） */
export const HOMEPAGE_HTML = `<!DOCTYPE html>
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
</html>`;

/** 需要剥离的请求头（由运行时或目标域名自行决定） */
const DROPPED_REQUEST_HEADERS = ['host', 'content-length', 'accept-encoding'];

/** hop-by-hop 头：逐跳语义，不应透传（对齐 Go 版 hopHeaders） */
const HOP_BY_HOP_HEADERS = [
	'connection',
	'keep-alive',
	'proxy-authenticate',
	'proxy-authorization',
	'te',
	'trailer',
	'transfer-encoding',
	'upgrade',
];

/** 请求体/缓存应答的最大字节数（对齐 Go 版 maxBodyBytes） */
const MAX_BODY_BYTES = 64 * 1024;

/** 单次上游请求超时（对齐 Go 版 client 15s） */
const UPSTREAM_TIMEOUT_MS = 15000;

/**
 * 读取主机白名单
 * 配置 ALLOWED_HOSTS（逗号分隔）后，只允许这些 Host 访问，
 * 其余（例如 *.workers.dev、*.pages.dev、预览域名）一律返回伪装 404。
 * @param {Object} [env]
 * @returns {string[]|null} null 表示不限制
 */
export function getAllowedHosts(env) {
	const raw = env && env.ALLOWED_HOSTS;
	if (!raw) return null;
	const list = Array.isArray(raw) ? raw : String(raw).split(',');
	const hosts = list.map((h) => String(h).trim().toLowerCase()).filter(Boolean);
	return hosts.length > 0 ? hosts : null;
}

/**
 * 判断本次请求的 Host 是否在白名单内
 * @param {string} hostname
 * @param {Object} [env]
 * @returns {boolean}
 */
export function isHostAllowed(hostname, env) {
	const hosts = getAllowedHosts(env);
	if (!hosts) return true;
	return hosts.includes(String(hostname).toLowerCase());
}

/**
 * 计算响应的缓存策略
 *
 * Workers Cache 开启后，响应若不带 Cache-Control 会走 RFC 9111 启发式缓存
 * （200 默认缓存 2 小时），对 DoH 是灾难，因此这里对每条响应都显式声明：
 *   - 仅 GET + Accept: application/dns-message + CACHE_TTL>0 才允许短缓存
 *   - 其余（含所有 POST、伪装 404）一律 no-store
 *
 * 注意：这只控制响应头（给浏览器/CDN 用的提示）；进程内 LRU 应答缓存由
 * CACHE_MAX_TTL 独立控制（POST 也会有进程内缓存，与 Go 版一致）。
 * @param {Request} request
 * @param {Object} [env]
 * @returns {{value: string, vary: boolean}}
 */
export function cachePolicy(request, env) {
	const ttl = Number((env && env.CACHE_TTL) || 0);
	const method = (request.method || 'GET').toUpperCase();
	const accept = request.headers.get('accept') || '';
	if (method === 'GET' && Number.isFinite(ttl) && ttl > 0 && accept.includes('application/dns-message')) {
		return { value: `public, max-age=${Math.floor(ttl)}`, vary: true };
	}
	return { value: 'no-store', vary: false };
}

/**
 * 读取路径映射配置
 * 优先使用环境变量 DOMAIN_MAPPINGS（JSON 字符串或对象），否则回退默认值
 * @param {Object} [env] 平台环境变量对象（Cloudflare env / EdgeOne context.env）
 * @returns {Object} 路径映射配置
 */
export function getPathMappings(env) {
	try {
		if (env && env.DOMAIN_MAPPINGS) {
			if (typeof env.DOMAIN_MAPPINGS === 'string') {
				return JSON.parse(env.DOMAIN_MAPPINGS);
			}
			return env.DOMAIN_MAPPINGS;
		}
	} catch (error) {
		console.error('Error accessing DOMAIN_MAPPINGS variable:', error);
	}

	return DEFAULT_PATH_MAPPINGS;
}

/** 根路径直连 DoH 的别名（多数 DoH 客户端默认就用 /dns-query） */
const BARE_DOH_PATHS = ['/dns-query', '/query-dns'];

/**
 * 构造「无路径前缀」的默认 DoH 目标
 * @param {URL} url
 * @param {Object} [env]
 * @returns {{prefix: string, targetDomain: string, targetUrl: string}|null}
 */
export function bareDohTarget(url, env) {
	// 小写归一对齐 Go（loadConfig 对 DEFAULT_UPSTREAM 做 ToLower），避免大写配置 404
	const name = String((env && env.DEFAULT_UPSTREAM) || 'cloudflare').replace(/^\/+/, '').toLowerCase();
	const mapping = getPathMappings(env)[`/${name}`];
	if (!mapping || !mapping.targetDomain) return null;
	return {
		prefix: '/',
		targetDomain: mapping.targetDomain,
		targetUrl: `https://${mapping.targetDomain}/dns-query${url.search}`,
	};
}

/**
 * 判断请求是否真的是 DoH 请求（用于根路径兜底，避免把浏览器访问也放行）
 * @param {Request} request
 * @returns {boolean}
 */
export function isDohRequest(request) {
	const accept = (request.headers.get('accept') || '').toLowerCase();
	const contentType = (request.headers.get('content-type') || '').toLowerCase();
	return accept.includes('application/dns-message') || contentType.includes('application/dns-message');
}

/**
 * 根据请求 URL 解析转发目标
 * @param {URL} url 原始请求 URL
 * @param {Object} [env] 平台环境变量对象
 * @returns {{prefix: string, targetDomain: string, targetUrl: string}|null} 未命中返回 null
 */
export function resolveTarget(url, env) {
	const path = url.pathname;
	const pathMappings = getPathMappings(env);

	// 最长前缀优先，避免 '/google' 抢先匹配 '/google-v2'
	const pathPrefix = Object.keys(pathMappings)
		.filter((prefix) => path.startsWith(prefix))
		.sort((a, b) => b.length - a.length)[0];

	if (!pathPrefix) {
		// 兼容 https://域名/dns-query 这种「无前缀」的标准 DoH 写法
		const bare = path.replace(/\/+$/, '');
		return BARE_DOH_PATHS.includes(bare) ? bareDohTarget(url, env) : null;
	}

	const mapping = pathMappings[pathPrefix] || {};
	if (!mapping.targetDomain) {
		return null;
	}

	const remainingPath = path.substring(pathPrefix.length);
	const pathMapping = mapping.pathMapping || {};

	// 按键长度倒序匹配，保证最长匹配（对齐 Go 版的 keys 排序）；
	// 只做「前缀」重写——目标串出现在中间时不得误替换（对齐 Go 版 HasPrefix + 拼接）
	let targetPath = remainingPath;
	const rewriteRules = Object.entries(pathMapping).sort((a, b) => b[0].length - a[0].length);
	for (const [sourcePath, destPath] of rewriteRules) {
		if (remainingPath.startsWith(sourcePath)) {
			targetPath = destPath + remainingPath.slice(sourcePath.length);
			break;
		}
	}

	// 前缀后无剩余路径时补根路径，避免拼出 https://dns.google?name=...
	if (!targetPath) {
		targetPath = '/';
	}

	return {
		prefix: pathPrefix,
		targetDomain: mapping.targetDomain,
		targetUrl: `https://${mapping.targetDomain}${targetPath}${url.search}`,
	};
}

/**
 * 构造伪装 404 响应
 * @returns {Response}
 */
export function camoResponse() {
	return new Response(HOMEPAGE_HTML, {
		status: 404,
		headers: {
			'Content-Type': 'text/html; charset=utf-8',
			// 显式禁止缓存：否则 Workers Cache 会按启发式给 404 缓存 3 分钟
			'Cache-Control': 'no-store',
			'X-Content-Type-Options': 'nosniff',
		},
	});
}

/**
 * 复制请求头，剥离由运行时接管、会引起歧义或 hop-by-hop 的头部
 * @param {Headers} headers
 * @returns {Headers}
 */
function forwardableHeaders(headers) {
	const result = new Headers();
	headers.forEach((value, key) => {
		const lower = key.toLowerCase();
		if (!DROPPED_REQUEST_HEADERS.includes(lower) && !HOP_BY_HOP_HEADERS.includes(lower)) {
			result.set(key, value);
		}
	});
	return result;
}

// ============================================================
// DNS wire format（Uint8Array，语义对齐 Go 版 parseDNSQuery/dnsMinTTL/...）
// ============================================================

/** 大端读 u32 */
function readBE32(b, off) {
	return (((b[off] << 24) | (b[off + 1] << 16) | (b[off + 2] << 8) | b[off + 3]) >>> 0);
}

/** 大端写 u32 */
function writeBE32(b, off, v) {
	b[off] = (v >>> 24) & 0xff;
	b[off + 1] = (v >>> 16) & 0xff;
	b[off + 2] = (v >>> 8) & 0xff;
	b[off + 3] = v & 0xff;
}

function bytesToHex(b) {
	let s = '';
	for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, '0');
	return s;
}

/**
 * 解析 DoH 查询报文，提取缓存键（对齐 Go parseDNSQuery）
 * 键 = DNS 头 flags(2B) + question 段（QNAME+QTYPE+QCLASS）；
 * 事务 ID 只返回不参与键（不同客户端 ID 不同但应答相同）。
 * @param {Uint8Array} body
 * @returns {{id: number, keyBytes: Uint8Array}|null} 解析失败返回 null
 */
export function parseDohMessageKey(body) {
	if (!body || body.length < 17) return null; // 12B 头 + 最短 question(根标签1B + QTYPE/QCLASS 4B)
	const qdcount = (body[4] << 8) | body[5];
	if (qdcount !== 1) return null; // 必须恰好 1 个 question
	let i = 12;
	while (i < body.length) {
		const l = body[i];
		if ((l & 0xc0) !== 0) return null; // question 不允许压缩指针
		i += 1;
		if (l === 0) break;
		i += l;
	}
	if (i + 4 > body.length) return null; // QTYPE+QCLASS 不完整
	const id = (body[0] << 8) | body[1];
	const keyBytes = new Uint8Array(2 + (i + 4 - 12));
	keyBytes[0] = body[2];
	keyBytes[1] = body[3];
	keyBytes.set(body.subarray(12, i + 4), 2);
	return { id, keyBytes };
}

/**
 * 跳过 QDCOUNT 个 question，返回应答区起点与 AN+NS+AR 总条数（对齐 Go dnsSectionStart）
 * @returns {{offset: number, count: number}|null}
 */
function dnsSectionStart(body) {
	if (body.length < 12) return null;
	const qd = (body[4] << 8) | body[5];
	const an = (body[6] << 8) | body[7];
	const ns = (body[8] << 8) | body[9];
	const ar = (body[10] << 8) | body[11];
	let i = 12;
	for (let c = 0; c < qd; c++) {
		while (i < body.length) {
			const l = body[i];
			if ((l & 0xc0) !== 0) {
				i += 2; // 指针占 2 字节并终结名字
				break;
			}
			i += 1;
			if (l === 0) break;
			i += l;
		}
		i += 4; // QTYPE + QCLASS
	}
	if (i > body.length) return null;
	return { offset: i, count: an + ns + ar };
}

/** 跳过记录名（支持压缩指针），返回名字结束偏移；失败 -1 */
function skipName(body, i) {
	while (i < body.length) {
		const l = body[i];
		if ((l & 0xc0) === 0xc0) return i + 2; // 压缩指针：占 2 字节并终止名字
		if ((l & 0xc0) !== 0) return -1; // 0x40/0x80 开头的字节不是合法标签长度（对齐 Go：解析失败）
		i += 1;
		if (l === 0) return i;
		i += l;
	}
	return -1;
}

/** 伪记录：OPT(41) 的 TTL 字段是扩展 RCODE/标志位，TKEY(249)/TSIG(250) 同理不可动 */
function isPseudoRR(type) {
	return type === 41 || type === 249 || type === 250;
}

/**
 * 读取偏移 i 处的一条资源记录（对齐 Go rrNext）
 * @returns {{type: number, ttlOff: number, next: number}|null}
 */
function rrNext(body, i) {
	const nameEnd = skipName(body, i);
	if (nameEnd < 0 || nameEnd + 10 > body.length) return null; // 固定头 10B：type2+class2+ttl4+rdlen2
	const type = (body[nameEnd] << 8) | body[nameEnd + 1];
	const ttlOff = nameEnd + 4;
	const rdlen = (body[nameEnd + 8] << 8) | body[nameEnd + 9];
	const next = nameEnd + 10 + rdlen;
	if (next > body.length) return null;
	return { type, ttlOff, next };
}

/**
 * 应答报文的最小记录 TTL（跳过伪记录）；解析失败返回 -1（对齐 Go dnsMinTTL）
 * @param {Uint8Array} body
 * @returns {number}
 */
export function dnsMinTTL(body) {
	const sec = dnsSectionStart(body);
	if (!sec) return -1;
	let min = -1;
	let i = sec.offset;
	for (let c = 0; c < sec.count; c++) {
		const rr = rrNext(body, i);
		if (!rr) return -1;
		if (!isPseudoRR(rr.type)) {
			const ttl = readBE32(body, rr.ttlOff);
			if (min < 0 || ttl < min) min = ttl;
		}
		i = rr.next;
	}
	return min;
}

/**
 * 按已过秒数递减应答区所有记录的 TTL（拷贝后修改，跳过伪记录，钳 0 下限）
 * 解析失败时原样返回入参报文——宁可 TTL 偏大也不破坏应答（对齐 Go decrementTTLs）
 * @param {Uint8Array} body
 * @param {number} sec 已过秒数
 * @returns {Uint8Array}
 */
export function decrementTTLs(body, sec) {
	const out = body.slice();
	const start = dnsSectionStart(out);
	if (!start) return body;
	let i = start.offset;
	for (let c = 0; c < start.count; c++) {
		const rr = rrNext(out, i);
		if (!rr) return body;
		if (!isPseudoRR(rr.type)) {
			const ttl = readBE32(out, rr.ttlOff);
			writeBE32(out, rr.ttlOff, ttl > sec ? ttl - sec : 0);
		}
		i = rr.next;
	}
	return out;
}

/** 把应答报文的事务 ID 改写为客户端 ID（对齐 Go rewriteID） */
export function rewriteID(body, id) {
	const out = body.slice();
	out[0] = (id >> 8) & 0xff;
	out[1] = id & 0xff;
	return out;
}

/** base64url / base64 解码 ?dns= 参数（无填充优先，带填充兜底） */
function decodeDnsParam(raw) {
	if (!raw) return null;
	const normalized = String(raw).replace(/-/g, '+').replace(/_/g, '/');
	const attempts = [normalized, normalized + '='.repeat((4 - (normalized.length % 4)) % 4)];
	for (const s of attempts) {
		try {
			const bin = atob(s);
			const out = new Uint8Array(bin.length);
			for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
			return out;
		} catch {
			// 继续尝试下一种填充
		}
	}
	return null;
}

/**
 * 从请求中提取 DNS 查询（对齐 Go dohQueryOf）
 * 非GET/HEAD：优先解析请求体（解析失败不回退）；体为空时回退 ?dns= 参数。
 * GET/HEAD：解析 ?dns= 参数。解析失败返回 null（不缓存、不报错），
 * JSON DoH 拿不到 wire 报文同样返回 null。
 * @param {Request} request
 * @param {URL} url
 * @param {Uint8Array|null} bodyBuf 已读取的请求体（GET/HEAD 传 null）
 * @returns {{id: number, keyBytes: Uint8Array}|null}
 */
function parseDohQuery(request, url, bodyBuf) {
	const method = (request.method || 'GET').toUpperCase();
	if (method !== 'GET' && method !== 'HEAD') {
		if (bodyBuf && bodyBuf.byteLength > 0) {
			return parseDohMessageKey(bodyBuf);
		}
		return parseDohMessageKey(decodeDnsParam(url.searchParams.get('dns'))); // 体为空：对齐 Go 回退 ?dns=
	}
	return parseDohMessageKey(decodeDnsParam(url.searchParams.get('dns')));
}

// ============================================================
// isolate 级运行状态：应答缓存（LRU）+ 上游熔断 + 计数器
// ============================================================

/**
 * 进程内 LRU 应答缓存（对齐 Go dnsCache：惰性过期 + 按条数淘汰）
 * @param {number} maxEntries 条目上限，<=0 返回 null（关闭）
 * @param {number} maxTtl 单条最长保留秒数，<=0 返回 null（关闭）
 */
function createDnsCache(maxEntries, maxTtl) {
	if (!maxEntries || maxEntries <= 0 || !maxTtl || maxTtl <= 0) return null;
	const map = new Map(); // key -> {body, contentType, fetchedAt, ttl}
	let hits = 0;
	let misses = 0;
	return {
		get(key) {
			const e = map.get(key);
			if (!e) {
				misses += 1;
				return null;
			}
			const elapsedSec = Math.floor((Date.now() - e.fetchedAt) / 1000);
			if (elapsedSec >= e.ttl) {
				map.delete(key);
				misses += 1;
				return null;
			}
			map.delete(key);
			map.set(key, e); // LRU 触碰
			hits += 1;
			return e;
		},
		put(key, body, contentType, ttl) {
			map.delete(key);
			map.set(key, { body, contentType: contentType || '', fetchedAt: Date.now(), ttl });
			while (map.size > maxEntries) {
				map.delete(map.keys().next().value);
			}
		},
		get hits() {
			return hits;
		},
		get misses() {
			return misses;
		},
		get size() {
			return map.size;
		},
	};
}

/**
 * 上游熔断状态（对齐 Go providerHealth：无显式 half-open，冷却到期即放行）
 * @param {number} threshold 连续失败阈值
 * @param {number} cooldownMs 冷却毫秒
 */
function createProviderHealth(threshold, cooldownMs) {
	const map = new Map(); // name -> {fails, until}
	const get = (name) => {
		let s = map.get(name);
		if (!s) {
			s = { fails: 0, until: 0 };
			map.set(name, s);
		}
		return s;
	};
	return {
		allow(name) {
			if (threshold <= 0) return true;
			const s = get(name);
			if (s.fails < threshold) return true;
			return Date.now() > s.until; // 冷却到期 → 放行探测
		},
		record(name, ok) {
			const s = get(name);
			if (ok) {
				s.fails = 0;
				return;
			}
			s.fails += 1;
			if (threshold > 0 && s.fails >= threshold) {
				s.until = Date.now() + cooldownMs;
			}
		},
		// 预登记一个上游（对齐 Go：/metrics 展示全部映射上游，而不仅是访问过的）
		touch(name) {
			get(name);
		},
		snapshot() {
			const out = {};
			for (const [name, s] of map) {
				out[name] = { healthy: s.fails < threshold, consecutive_failures: s.fails };
			}
			return out;
		},
	};
}

/**
 * 创建 isolate 级运行状态（每个 isolate 首个请求时创建一次；测试可按需新建）
 * 环境变量读取一次并固化：CACHE_MAX_TTL / CACHE_ENTRIES / CB_THRESHOLD /
 * CB_COOLDOWN_SEC / FAILOVER_CHAIN
 * @param {Object} [env]
 */
export function createEdgeState(env = {}) {
	const num = (v, d) => {
		const n = Number(v);
		return Number.isFinite(n) && n >= 0 ? n : d;
	};
	const cacheMaxTtl = num(env.CACHE_MAX_TTL, 300);
	const cacheEntries = num(env.CACHE_ENTRIES, 10000);
	const cbThreshold = num(env.CB_THRESHOLD, 5);
	const cbCooldownSec = num(env.CB_COOLDOWN_SEC, 30);
	// 空字符串 = 显式禁用故障转移（与 Go 版 strings.Split 语义一致），仅未设置时取默认
	const chainRaw = env.FAILOVER_CHAIN === undefined || env.FAILOVER_CHAIN === null ? 'cloudflare,google,adguard' : String(env.FAILOVER_CHAIN);
	const failoverChain = chainRaw
		.split(',')
		.map((s) => s.trim().toLowerCase().replace(/^\/+/, ''))
		.filter(Boolean);
	const health = createProviderHealth(cbThreshold, cbCooldownSec * 1000);
	// 预登记全部映射上游：/metrics 的 providers 覆盖所有上游（对齐 Go newServer）
	for (const name of Object.keys(getPathMappings(env))) {
		health.touch(name.replace(/^\//, ''));
	}
	return {
		cache: createDnsCache(cacheMaxTtl > 0 ? cacheEntries : 0, cacheMaxTtl),
		cacheMaxTtl,
		health,
		failoverChain,
		startedAt: Date.now(),
		totalRequests: 0,
		upstreamErrors: 0,
	};
}

/** isolate 级共享状态（懒创建；注入 options.state 可覆盖，测试用） */
let sharedState = null;
function stateFor(env) {
	if (!sharedState) sharedState = createEdgeState(env);
	return sharedState;
}

/** 上游名 → 转移目标 URL（恒为 /dns-query + 原查询串，对齐 Go providerTarget） */
function failoverTarget(name, url, env) {
	const mapping = getPathMappings(env)[`/${name}`];
	if (!mapping || !mapping.targetDomain) return null;
	return `https://${mapping.targetDomain}/dns-query${url.search}`;
}

/**
 * 计算本次请求的上游候选链（对齐 Go candidates）：
 * [primary(若 allow)] + FAILOVER_CHAIN 中健康且存在的其它上游；全 OPEN 时仍打 primary
 */
function failoverCandidates(primary, state, env) {
	const mappings = getPathMappings(env);
	const out = [];
	if (state.health.allow(primary)) out.push(primary);
	for (const name of state.failoverChain) {
		if (name === primary || out.includes(name)) continue;
		const mapping = mappings[`/${name}`];
		if (!mapping || !mapping.targetDomain) continue; // 不在映射表里的名字忽略
		if (state.health.allow(name)) out.push(name);
	}
	if (out.length === 0) out.push(primary);
	return out;
}

/** 纯文本应答（错误/健康检查共用；nosniff 对齐 Go http.Error） */
function plainResponse(status, text, extraHeaders = {}) {
	return new Response(text, {
		status,
		headers: {
			'Content-Type': 'text/plain; charset=utf-8',
			'Cache-Control': 'no-store',
			'X-Content-Type-Options': 'nosniff',
			...extraHeaders,
		},
	});
}

/** /metrics JSON（对齐 Go writeMetrics；省略进程级独有字段） */
function metricsResponse(state) {
	const payload = {
		total_requests: state.totalRequests,
		upstream_errors: state.upstreamErrors,
		uptime_seconds: Math.floor((Date.now() - state.startedAt) / 1000),
		providers: state.health.snapshot(),
	};
	if (state.cache) {
		payload.cache_hits = state.cache.hits;
		payload.cache_misses = state.cache.misses;
		payload.cache_entries = state.cache.size;
		payload.cache_max_ttl = state.cacheMaxTtl;
	} else {
		payload.cache_entries = 0;
	}
	return new Response(JSON.stringify(payload), {
		status: 200,
		headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' },
	});
}

/**
 * 核心代理：命中映射则转发到目标 DoH 服务，否则返回伪装 404。
 * 处理顺序对齐 Go 版 ServeHTTP：白名单 → healthz/metrics → 目标解析 →
 * 读请求体 → 缓存命中即返回 → 上游候选链（熔断+故障转移）→ 存缓存 → 响应。
 * @param {Request} request 原始请求
 * @param {Object} [options]
 * @param {Object} [options.env] 平台环境变量对象
 * @param {Function} [options.fetchImpl] 自定义 fetch 实现（便于测试）
 * @param {Object} [options.state] 运行状态（默认 isolate 级共享；测试注入全新状态）
 * @returns {Promise<Response>}
 */
export async function proxyRequest(request, options = {}) {
	const { env, fetchImpl = fetch } = options;
	const state = options.state || stateFor(env);
	state.totalRequests += 1;

	const url = new URL(request.url);

	// 主机白名单：未配置则放行；配置后只有绑定域名可用，
	// workers.dev / pages.dev / 预览域名都会被这条挡住
	if (!isHostAllowed(url.hostname, env)) {
		return camoResponse();
	}

	// 健康检查 / 指标端点（精确路径匹配，未配置时走下方伪装 404）
	const healthPath = env && env.HEALTH_PATH ? String(env.HEALTH_PATH) : '';
	if (healthPath && url.pathname === healthPath) {
		return plainResponse(200, 'ok');
	}
	const metricsPath = env && env.METRICS_PATH ? String(env.METRICS_PATH) : '';
	if (metricsPath && url.pathname === metricsPath) {
		return metricsResponse(state);
	}

	let target = resolveTarget(url, env);

	// 根路径兜底：只有「看起来确实是 DoH 请求」才转发，
	// 浏览器直接访问 https://域名/ 仍返回伪装 404
	if (!target && (url.pathname === '/' || url.pathname === '') && isDohRequest(request)) {
		target = bareDohTarget(url, env);
	}

	if (!target) {
		return camoResponse();
	}

	const method = (request.method || 'GET').toUpperCase();

	// 读取请求体（GET/HEAD 无体；上限对齐 Go maxBodyBytes；读体失败返回 400 对齐 Go）
	let bodyBuf = null;
	if (method !== 'GET' && method !== 'HEAD') {
		try {
			bodyBuf = new Uint8Array(await request.arrayBuffer());
		} catch (error) {
			return plainResponse(400, 'bad request body');
		}
		if (bodyBuf.byteLength > MAX_BODY_BYTES) {
			return plainResponse(413, 'request body too large');
		}
	}

	// 缓存键：provider 名 + DNS 报文键；JSON DoH / 解析失败 → 无键（不缓存、无 X-Cache）
	const provider =
		target.prefix === '/'
			? String((env && env.DEFAULT_UPSTREAM) || 'cloudflare').replace(/^\/+/, '').toLowerCase()
			: target.prefix.slice(1);
	const parsed = parseDohQuery(request, url, bodyBuf);
	const cacheKey = parsed && state.cache ? `${provider}|${bytesToHex(parsed.keyBytes)}` : null;

	// 缓存命中：直接本地返回（对齐 Go：熔断不影响命中路径）
	if (cacheKey && state.cache) {
		const entry = state.cache.get(cacheKey);
		if (entry) {
			const elapsedSec = Math.floor((Date.now() - entry.fetchedAt) / 1000);
			let out = decrementTTLs(entry.body, elapsedSec);
			out = rewriteID(out, parsed.id);
			const policy = cachePolicy(request, env);
			const headers = new Headers();
			headers.set('Content-Type', entry.contentType || 'application/dns-message');
			headers.set('Cache-Control', policy.value);
			if (policy.vary) headers.set('Vary', 'Accept');
			headers.set('X-Cache', 'HIT');
			return new Response(out, { status: 200, headers });
		}
	}

	// 上游候选链：失败（网络错误/超时/5xx）逐个转移；4xx 算成功原样透传
	const candidates = failoverCandidates(provider, state, env);
	let upstream = null; // 成功的上游响应
	let bufferedBody = null; // 已整读的响应体（入缓存时使用）
	let lastUpstreamStatus = 0; // 最后一次上游 5xx 状态码

	for (const cand of candidates) {
		const attemptUrl = cand === provider ? target.targetUrl : failoverTarget(cand, url, env);
		if (!attemptUrl) continue;
		const init = {
			method,
			headers: forwardableHeaders(request.headers),
			redirect: 'follow',
		};
		if (bodyBuf && bodyBuf.byteLength > 0) {
			init.body = bodyBuf;
		}
		if (typeof AbortSignal === 'function' && typeof AbortSignal.timeout === 'function') {
			init.signal = AbortSignal.timeout(UPSTREAM_TIMEOUT_MS);
		}

		let resp = null;
		try {
			resp = await fetchImpl(attemptUrl, init);
		} catch (error) {
			state.health.record(cand, false);
			continue;
		}
		if (resp.status >= 500) {
			state.health.record(cand, false);
			lastUpstreamStatus = resp.status;
			try {
				await resp.arrayBuffer(); // 消费掉以便连接复用
			} catch {
				// 忽略读体失败
			}
			continue;
		}

		state.health.record(cand, true);

		// 200 + 有缓存键 → 整读响应体并尝试入缓存（对齐 Go storeCache 条件）；
		// 读体失败（罕见：上游中途断流/连接中断）→ 记该上游失败，继续候选链
		if (cacheKey && resp.status === 200) {
			try {
				bufferedBody = new Uint8Array(await resp.arrayBuffer());
				if (bufferedBody.byteLength <= MAX_BODY_BYTES) {
					const rcode = bufferedBody[3] & 0x0f;
					if (rcode === 0 || rcode === 3) {
						const minTtl = dnsMinTTL(bufferedBody);
						if (minTtl > 0) {
							state.cache.put(cacheKey, bufferedBody, resp.headers.get('content-type') || '', Math.min(minTtl, state.cacheMaxTtl));
						}
					}
				}
			} catch (error) {
				state.health.record(cand, false);
				resp = null;
			}
		}
		if (!resp) continue;
		upstream = resp;
		break;
	}

	// 全部候选失败：最后一次是 5xx → 503，网络错误 → 502（对齐 Go）
	if (!upstream) {
		state.upstreamErrors += 1;
		return plainResponse(lastUpstreamStatus ? 503 : 502, 'upstream error\n', { 'Retry-After': '2' });
	}

	// 响应头：复制上游 → 剥 hop-by-hop → 覆盖缓存策略 → X-Cache
	// （set-cookie 用 append 保留多条，对齐 Go 的 Add 语义）
	const policy = cachePolicy(request, env);
	const headers = new Headers();
	upstream.headers.forEach((value, key) => {
		if (HOP_BY_HOP_HEADERS.includes(key.toLowerCase())) return;
		if (key.toLowerCase() === 'set-cookie') {
			headers.append(key, value);
		} else {
			headers.set(key, value);
		}
	});
	headers.set('Cache-Control', policy.value);
	if (policy.vary) {
		const vary = headers.get('Vary');
		headers.set('Vary', vary ? `${vary}, Accept` : 'Accept');
	}
	if (cacheKey) {
		headers.set('X-Cache', 'MISS');
	}

	return new Response(bufferedBody || upstream.body, {
		status: upstream.status,
		statusText: upstream.statusText,
		headers,
	});
}

export default {
	resolveTarget,
	bareDohTarget,
	isDohRequest,
	getPathMappings,
	getAllowedHosts,
	isHostAllowed,
	cachePolicy,
	proxyRequest,
	camoResponse,
	parseDohMessageKey,
	dnsMinTTL,
	decrementTTLs,
	rewriteID,
	createEdgeState,
	HOMEPAGE_HTML,
	DEFAULT_PATH_MAPPINGS,
};
