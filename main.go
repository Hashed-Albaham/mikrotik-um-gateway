// خدمة صغيرة تعمل عند الطلب فقط: تقرأ بيانات الكرت من بوابة User Manager في
// MikroTik RouterOS v7 وتعيد الرصيد المتبقي والوقت والصلاحية بصيغة JSON.
//
// لا حلقات خلفية ولا ذاكرة مؤقتة: كل طلب يفتح جلسة خاصة به، يقرأ البيانات في
// طلبين متوازيين، يحسب المتبقي، ثم يغلق الجلسة بعد مدة قصيرة قابلة للضبط.
// كل الإعدادات تُقرأ من متغيرات البيئة: العنوان، البروتوكول، البورت، المنفذ.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// الإعدادات
// ---------------------------------------------------------------------------

type config struct {
	portalURL   string        // عنوان بوابة اليوزر منجر
	listenAddr  string        // منفذ الخدمة المحلية
	logoutDelay time.Duration // التأخير قبل إغلاق جلسة البوابة
	timeout     time.Duration // مهلة تسجيل الدخول والقراءة
}

var cfg = loadConfig()

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envSeconds(key, fallback string) time.Duration {
	number, err := strconv.Atoi(env(key, fallback))
	if err != nil || number < 0 {
		number, _ = strconv.Atoi(fallback)
	}
	return time.Duration(number) * time.Second
}

func loadConfig() config {
	// العنوان: إما UM_TARGET_URL كاملاً، أو UM_PROTO + UM_HOST + UM_PORT.
	target := strings.TrimSpace(os.Getenv("UM_TARGET_URL"))
	if target == "" {
		target = fmt.Sprintf("%s://%s:%s",
			strings.ToLower(env("UM_PROTO", "http")),
			env("UM_HOST", "192.168.216.1"),
			env("UM_PORT", "12008"))
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	// منفذ الخدمة: LISTEN_ADDR أولاً، ثم PORT.
	listen := env("LISTEN_ADDR", "")
	if listen == "" {
		listen = ":" + env("PORT", "8080")
	}

	return config{
		portalURL:   strings.TrimRight(target, "/"),
		listenAddr:  listen,
		logoutDelay: envSeconds("LOGOUT_DELAY_SECONDS", "60"),
		timeout:     envSeconds("REQUEST_TIMEOUT_SECONDS", "8"),
	}
}

// ---------------------------------------------------------------------------
// أشكال الاستجابة: ما يخرج للعميل، وما يصل من البوابة.
// ---------------------------------------------------------------------------

type statusResponse struct {
	Success           bool   `json:"success"`
	MaskedUsername    string `json:"masked_username"`
	ProfileName       string `json:"profile_name,omitempty"`
	ProfileState      string `json:"profile_state"`
	DataLeft          string `json:"data_left"`
	TotalData         string `json:"total_data"`
	TimeLeft          string `json:"time_left"`
	TotalTime         string `json:"total_time"`
	RemainingValidity string `json:"remaining_validity"`
	UsedDownload      string `json:"used_download"`
	UsedUpload        string `json:"used_upload"`
	UsedUptime        string `json:"used_uptime"`
	ActiveSessions    int    `json:"active_sessions"`
	SharedUsers       int    `json:"shared_users"`
	Error             string `json:"error,omitempty"`
}

// GET /um/api/getUser
type accountResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Username       string `json:"username"`
		Download       string `json:"download"`
		Upload         string `json:"upload"`
		Uptime         string `json:"uptime"`
		ActiveSessions int    `json:"activeSess"`
		SharedUsers    int    `json:"sharedUsers"`
	} `json:"data"`
}

type profileEntry struct {
	Name     string          `json:"name"`
	State    json.RawMessage `json:"state"`
	ExpAfter string          `json:"expAfter"`
}

// GET /um/api/getUserProfiles
type profilesResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Profiles []profileEntry `json:"profiles"`
	} `json:"data"`
}

// ---------------------------------------------------------------------------
// المعالج: دخول، قراءتان متوازيتان، حساب، ثم إغلاق مؤجل.
// ---------------------------------------------------------------------------

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	user := firstValue(r, "user", "username")
	password := firstValue(r, "pass", "password")
	if user == "" {
		writeJSON(w, http.StatusBadRequest, statusResponse{Success: false, Error: "المرجع user مطلوب"})
		return
	}

	client, err := newPortalClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusResponse{Success: false, Error: "تعذر تهيئة عميل البوابة"})
		return
	}

	if err := login(client, user, password); err != nil {
		log.Printf("فشل الدخول لـ %s: %v", maskUsername(user), err)
		writeJSON(w, http.StatusUnauthorized, statusResponse{
			Success: false, MaskedUsername: maskUsername(user), Error: err.Error(),
		})
		return
	}

	// قراءتان متوازيتان: بيانات الكرت، والبروفايلات المرتبطة به.
	var (
		account    accountResponse
		profiles   profilesResponse
		accountErr error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		accountErr = getJSON(client, "/um/api/getUser", &account)
	}()
	go func() {
		defer wg.Done()
		// فشل قراءة البروفايلات لا يُفشل الطلب: يُعاد ما توفر من بيانات.
		_ = getJSON(client, "/um/api/getUserProfiles", &profiles)
	}()
	wg.Wait()

	// إغلاق الاتصالات الخاملة فوراً حتى لا تبقى مفتوحة أثناء التأخير قبل الخروج.
	client.CloseIdleConnections()
	scheduleLogout(client, user)

	if accountErr != nil {
		log.Printf("تعذر قراءة بيانات %s: %v", maskUsername(user), accountErr)
		writeJSON(w, http.StatusBadGateway, statusResponse{
			Success: false, MaskedUsername: maskUsername(user), Error: "تعذر قراءة البيانات من اليوزر منجر",
		})
		return
	}

	response := buildResponse(user, account, profiles)
	writeJSON(w, http.StatusOK, response)
	log.Printf("%s: المتبقي %s / %s", response.MaskedUsername, response.DataLeft, response.TimeLeft)
}

// أول قيمة موجودة من المرجع المطلوب، في الرابط أو في جسم الطلب.
func firstValue(r *http.Request, keys ...string) string {
	if err := r.ParseForm(); err != nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(r.Form.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// التعامل مع البوابة
// ---------------------------------------------------------------------------

func newPortalClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	verify := !strings.EqualFold(env("UM_INSECURE_SKIP_VERIFY", "true"), "false")
	return &http.Client{
		Jar:     jar,
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			DisableCompression:  true,
			// بوابة الراوتر عادةً بشهادة موقعة ذاتياً؛ UM_INSECURE_SKIP_VERIFY=false لإلزام التحقق.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify}, //nolint:gosec
		},
	}, nil
}

func portalURL(path string) string { return cfg.portalURL + path }

// تسجيل الدخول يُرسل بصيغة نموذج كما تتوقع البوابة.
func login(client *http.Client, user, password string) error {
	form := url.Values{"username": {user}, "password": {password}}
	response, err := client.PostForm(portalURL("/um/api/login"), form)
	if err != nil {
		return fmt.Errorf("تعذر الوصول إلى البوابة")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("بيانات الدخول غير صحيحة")
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("رد غير مفهوم من البوابة")
	}
	if !result.Success {
		return fmt.Errorf("بيانات الدخول غير صحيحة")
	}
	return nil
}

func getJSON(client *http.Client, path string, target any) error {
	response, err := client.Get(portalURL(path))
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s أعاد الرمز %d", path, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

// إغلاق الجلسة بعد مدة قصيرة، بمؤقت واحد فقط لكل طلب.
func scheduleLogout(client *http.Client, user string) {
	if cfg.logoutDelay <= 0 {
		return
	}
	time.AfterFunc(cfg.logoutDelay, func() {
		body := strings.NewReader(url.Values{"username": {user}}.Encode())
		request, err := http.NewRequest(http.MethodPost, portalURL("/um/api/logout"), body)
		if err == nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if response, err := client.Do(request.WithContext(ctx)); err == nil {
				_ = response.Body.Close()
			}
		}
		client.CloseIdleConnections()
	})
}

// ---------------------------------------------------------------------------
// الحساب
// ---------------------------------------------------------------------------

func buildResponse(user string, account accountResponse, profiles profilesResponse) statusResponse {
	download := parseSize(account.Data.Download)
	upload := parseSize(account.Data.Upload)
	uptime := parseSpan(account.Data.Uptime)

	response := statusResponse{
		Success:        true,
		MaskedUsername: maskUsername(firstNonEmpty(account.Data.Username, user)),
		ProfileState:   "غير معروف",
		UsedDownload:   account.Data.Download,
		UsedUpload:     account.Data.Upload,
		UsedUptime:     account.Data.Uptime,
		ActiveSessions: account.Data.ActiveSessions,
		SharedUsers:    account.Data.SharedUsers,
	}

	// يُفضَّل أول بروفايل غير منتهٍ، لأن الكرت قد يحمل أكثر من بروفايل.
	profile, expired := selectProfile(profiles.Data.Profiles)
	response.ProfileName = profile.Name
	switch {
	case expired:
		response.ProfileState = "منتهي"
	case profile.Name != "":
		response.ProfileState = "نشط"
	}

	dataLimit, dataUnlimited, timeLimit, timeUnlimited := parsePlan(profile.Name)

	switch {
	case dataUnlimited:
		response.DataLeft, response.TotalData = "غير محدود", "غير محدود"
	case dataLimit > 0:
		response.DataLeft = formatBytes(remaining(dataLimit, download+upload))
		response.TotalData = formatBytes(dataLimit)
	default:
		response.DataLeft, response.TotalData = "غير معروف", "غير معروف"
	}

	switch {
	case timeUnlimited:
		response.TimeLeft, response.TotalTime = "غير محدود", "غير محدود"
	case timeLimit > 0:
		response.TimeLeft = formatSpan(remaining(timeLimit, uptime))
		response.TotalTime = formatSpan(timeLimit)
	default:
		response.TimeLeft, response.TotalTime = "غير معروف", "غير معروف"
	}

	// الصلاحية المتبقية: من البوابة إن وُجدت، وإلا ما تبقى من وقت الخطة.
	response.RemainingValidity = response.TimeLeft
	if isUnlimited(profile.ExpAfter) {
		response.RemainingValidity = "غير محدود"
	} else if seconds, ok := parseValidity(profile.ExpAfter); ok {
		response.RemainingValidity = formatSpan(seconds)
	}

	if expired {
		response.ProfileState = "منتهي"
		response.TimeLeft, response.RemainingValidity = "0", "0"
	}
	return response
}

// selectProfile يعيد البروفايل المستخدم، ويشير إلى أنه منتهٍ إن كانت كلها منتهية.
func selectProfile(profiles []profileEntry) (profileEntry, bool) {
	var expiredEntry profileEntry
	for _, candidate := range profiles {
		if !isExpired(candidate.State) {
			return candidate, false
		}
		if expiredEntry.Name == "" {
			expiredEntry = candidate
		}
	}
	return expiredEntry, expiredEntry.Name != ""
}

func remaining(limit, used int64) int64 {
	if used > limit {
		return 0
	}
	return limit - used
}

// حالة البروفايل: 3 تعني مستهلك في اليوزر منجر.
func isExpired(state json.RawMessage) bool {
	if len(state) == 0 {
		return false
	}
	var number int
	if err := json.Unmarshal(state, &number); err == nil {
		return number == 3
	}
	var text string
	if err := json.Unmarshal(state, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "used", "expired", "stopped", "disabled":
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// تحليل صيغة البروفايل: السعر - البيانات - الوقت، مثل 1000-1G-8h
// ---------------------------------------------------------------------------

// parsePlan يستخرج حدود البيانات والوقت من اسم البروفايل:
//   - الوحدة الكبيرة K/M/G/T تعني بيانات، والصغيرة w/d/h/m/s تعني وقتاً.
//   - كلمة un/unlimited تعني غير محدود وتُسند للبيانات أولاً ثم للوقت.
//   - الأرقام المجردة (السعر) تُتجاهل.
func parsePlan(name string) (dataLimit int64, dataUnlimited bool, timeLimit int64, timeUnlimited bool) {
	for _, token := range strings.FieldsFunc(name, isTokenSeparator) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if isUnlimited(token) {
			if dataLimit == 0 && !dataUnlimited {
				dataUnlimited = true
			} else {
				timeUnlimited = true
			}
			continue
		}
		if size, ok := parseDataToken(token); ok {
			if dataLimit == 0 && !dataUnlimited {
				dataLimit = size
			}
			continue
		}
		if span, ok := parseTimeToken(token); ok {
			if timeLimit == 0 && !timeUnlimited {
				timeLimit = span
			}
		}
	}
	return dataLimit, dataUnlimited, timeLimit, timeUnlimited
}

func isTokenSeparator(r rune) bool { return r == '-' || r == '_' || r == '/' || r == ' ' || r == ',' }

// parseDataToken يقبل "1G" و"1GB" و"500M"، ويرفض الأرقام المجردة.
// الوحدة يجب أن تكون كبيرة (K/M/G/T) كما في صيغة اليوزر منجر "1000-1G-8h".
func parseDataToken(token string) (int64, bool) {
	text := strings.TrimSuffix(strings.TrimSpace(token), "B")
	if len(text) < 2 {
		return 0, false
	}
	multiplier, ok := sizeUnits[text[len(text)-1:]]
	if !ok {
		return 0, false
	}
	number, err := strconv.ParseFloat(text[:len(text)-1], 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return int64(number * float64(multiplier)), true
}

// parseTimeToken يقبل "8h" و"30m" و"1d" و"08:30:00".
func parseTimeToken(token string) (int64, bool) {
	text := strings.TrimSpace(token)
	if strings.Contains(text, ":") {
		return parseClock(text)
	}
	if !strings.ContainsAny(text, "wdhms") || strings.ContainsAny(text, "WDHMS") {
		return 0, false
	}
	return parseSpanStrict(text)
}

func parseClock(text string) (int64, bool) {
	parts := strings.Split(text, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err1 := strconv.ParseInt(parts[0], 10, 64)
	minutes, err2 := strconv.ParseInt(parts[1], 10, 64)
	seconds, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return hours*3600 + minutes*60 + seconds, true
}

// ---------------------------------------------------------------------------
// أدوات التحويل والتنسيق
// ---------------------------------------------------------------------------

var sizeUnits = map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}

// parseSize يحوّل "1.5G" أو "500M" أو "123456" إلى بايت.
func parseSize(text string) int64 {
	text = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(text), ",", ""))
	if text == "" {
		return 0
	}
	index := 0
	for index < len(text) && (isDigit(text[index]) || text[index] == '.') {
		index++
	}
	if index == 0 {
		return 0
	}
	number, err := strconv.ParseFloat(text[:index], 64)
	if err != nil {
		return 0
	}
	suffix := strings.TrimRight(text[index:], "B")
	if suffix == "" {
		return int64(number)
	}
	multiplier, ok := sizeUnits[suffix]
	if !ok {
		return 0
	}
	return int64(number * float64(multiplier))
}

// parseSpan يحوّل "2h30m" أو "1d" أو "08:30:00" إلى ثوانٍ.
func parseSpan(text string) int64 {
	text = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
	if text == "" {
		return 0
	}
	if span, ok := parseClock(text); ok {
		return span
	}
	span, _ := parseSpanStrict(text)
	return span
}

// parseSpanStrict يحوّل صيغة مثل "1d2h30m" إلى ثوانٍ.
func parseSpanStrict(text string) (int64, bool) {
	var total, number int64
	hasNumber, matched := false, false
	for index := 0; index < len(text); index++ {
		char := text[index]
		switch {
		case isDigit(char):
			number = number*10 + int64(char-'0')
			hasNumber = true
		case char == 'w' || char == 'd' || char == 'h' || char == 'm' || char == 's':
			if !hasNumber {
				return 0, false
			}
			switch char {
			case 'w':
				total += number * 604800
			case 'd':
				total += number * 86400
			case 'h':
				total += number * 3600
			case 'm':
				total += number * 60
			case 's':
				total += number
			}
			number, hasNumber, matched = 0, false, true
		default:
			return 0, false
		}
	}
	if hasNumber || !matched {
		return 0, false
	}
	return total, true
}

// parseValidity يقرأ expAfter: مدة متبقية أو تاريخ انتهاء.
func parseValidity(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text == "..." {
		return 0, false
	}
	if text == "-" || text == "0" {
		return 0, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if end, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			seconds := int64(time.Until(end).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			return seconds, true
		}
	}
	compact := strings.NewReplacer(" ", "", ",", "").Replace(strings.ToLower(text))
	return parseSpanStrict(compact)
}

func formatBytes(bytes int64) string {
	switch {
	case bytes >= 1<<40:
		return formatUnit(float64(bytes)/(1<<40), "TB")
	case bytes >= 1<<30:
		return formatUnit(float64(bytes)/(1<<30), "GB")
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatUnit يحذف الكسر الصفري فيظهر "1 GB" بدل "1.0 GB".
func formatUnit(value float64, unit string) string {
	text := strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
	return text + " " + unit
}

func formatSpan(seconds int64) string {
	if seconds <= 0 {
		return "0"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%ds", seconds)
	}
	return strings.Join(parts, " ")
}

// إخفاء رقم الكرت: 0018067158 تصبح 00******58
func maskUsername(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		return ""
	}
	if len(user) <= 4 {
		return strings.Repeat("*", len(user))
	}
	return user[:2] + strings.Repeat("*", len(user)-4) + user[len(user)-2:]
}

func isUnlimited(token string) bool {
	switch strings.ToUpper(strings.TrimSpace(token)) {
	case "UN", "UNL", "UNLTD", "UNLIMITED", "INF", "INFINITE":
		return true
	}
	return false
}

func isDigit(char byte) bool { return char >= '0' && char <= '9' }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ---------------------------------------------------------------------------
// التشغيل
// ---------------------------------------------------------------------------

func main() {
	http.HandleFunc("/api/user-status", handleStatus)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	log.Printf("الخدمة على %s | بوابة اليوزر منجر: %s | تأخير الخروج: %s",
		cfg.listenAddr, cfg.portalURL, cfg.logoutDelay)
	if err := http.ListenAndServe(cfg.listenAddr, nil); err != nil {
		log.Fatalf("توقفت الخدمة: %v", err)
	}
}
