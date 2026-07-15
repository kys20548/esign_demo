package main

import (
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var checkItemNames = []string{
	"外觀無變形、鏽蝕、積塵",
	"接地系統鎖固正常",
	"匯流排連接處無過熱痕跡",
	"絕緣電阻測試合格",
	"保護電驛動作測試正常",
	"盤內清潔、無異物",
}

type CheckItem struct {
	Name string
	OK   bool
}

type Form struct {
	ID           int
	TaskID       int // 0 表示臨時填單、未綁定派工
	Customer     string
	Site         string
	Panel        string
	Engineer     string
	Items        []CheckItem
	Note         string
	HasPhoto     bool
	Signature    template.URL // data:image/png;base64,... 客戶手寫簽名，未簽則為空字串；型別已標記為安全 URL（見 parseSignature 的驗證）
	Status       string       // 待簽核 / 已核准 / 已退回
	RejectReason string       // 退回時主管填寫，供工程師重新填單時參考
	CreatedAt    time.Time
	ReviewedAt   time.Time
}

const signaturePrefix = "data:image/png;base64,"

// parseSignature 驗證客戶端送來的簽名是合法的 base64 PNG data URL，並回傳
// template.URL 型別——html/template 預設會把 data: URI 當成不安全來源、
// 渲染成 #ZgotmplZ，只有明確標記為 template.URL 才會放行；只有在確認前綴
// 與 base64 內容都合法之後才做這個標記，避免真的引入 XSS 風險。
func parseSignature(sig string) template.URL {
	if !strings.HasPrefix(sig, signaturePrefix) || len(sig) > 2<<20 {
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(sig[len(signaturePrefix):]); err != nil {
		return ""
	}
	return template.URL(sig)
}

// IsAnomaly 代表本張單有檢測項目未通過，主管簽核時應特別留意。
func (f *Form) IsAnomaly() bool {
	for _, it := range f.Items {
		if !it.OK {
			return true
		}
	}
	return false
}

// Task 是一筆派工：某位工程師要去某客戶案場完成簽核。FormID=0 表示還沒填單。
type Task struct {
	ID       int
	Customer string
	Site     string
	Panel    string
	Engineer string
	FormID   int
}

type TaskView struct {
	*Task
	Status string
}

func (t TaskView) StatusClass() string { return statusClass(t.Status) }

func statusClass(status string) string {
	switch status {
	case "已核准":
		return "approved"
	case "已退回":
		return "rejected"
	case "待簽核":
		return "pending"
	default:
		return "todo"
	}
}

func (f *Form) CreatedFmt() string { return f.CreatedAt.Format("01/02 15:04") }
func (f *Form) ReviewedFmt() string {
	if f.ReviewedAt.IsZero() {
		return ""
	}
	return f.ReviewedAt.Format("01/02 15:04")
}

func (f *Form) StatusClass() string { return statusClass(f.Status) }

type store struct {
	mu         sync.Mutex
	forms      map[int]*Form
	photos     map[int][]byte
	photoType  map[int]string
	tasks      map[int]*Task
	nextID     int
	nextTaskID int
}

func newStore() *store {
	return &store{
		forms:      map[int]*Form{},
		photos:     map[int][]byte{},
		photoType:  map[int]string{},
		tasks:      map[int]*Task{},
		nextID:     1,
		nextTaskID: 1,
	}
}

func (s *store) add(f *Form, photo []byte, photoType string) *Form {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.ID = s.nextID
	s.nextID++
	if len(photo) > 0 {
		f.HasPhoto = true
		s.photos[f.ID] = photo
		s.photoType[f.ID] = photoType
	}
	s.forms[f.ID] = f
	return f
}

func (s *store) get(id int) (*Form, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	return f, ok
}

func (s *store) photo(id int) ([]byte, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.photos[id], s.photoType[id]
}

// list returns forms split into 待簽核 and 已處理, both newest first.
func (s *store) list() (pending, processed []*Form) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.forms {
		if f.Status == "待簽核" {
			pending = append(pending, f)
		} else {
			processed = append(processed, f)
		}
	}
	newestFirst := func(fs []*Form) {
		sort.Slice(fs, func(i, j int) bool { return fs[i].CreatedAt.After(fs[j].CreatedAt) })
	}
	newestFirst(pending)
	newestFirst(processed)
	return pending, processed
}

// formsForTask 回傳同一個派工底下所有送過的單，依填單時間由舊到新排序，
// 用來在詳情頁呈現「建立 → 退回 → 重新送單」的完整歷程。
func (s *store) formsForTask(taskID int) []*Form {
	if taskID == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Form
	for _, f := range s.forms {
		if f.TaskID == taskID {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Stats 給主管後台的儀表板：各狀態張數與異常項目張數。
type Stats struct {
	Pending, Approved, Rejected, Anomaly int
}

func (s *store) stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st Stats
	for _, f := range s.forms {
		switch f.Status {
		case "待簽核":
			st.Pending++
		case "已核准":
			st.Approved++
		case "已退回":
			st.Rejected++
		}
		if f.IsAnomaly() {
			st.Anomaly++
		}
	}
	return st
}

// clear empties all data; caller re-seeds afterwards.
func (s *store) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forms = map[int]*Form{}
	s.photos = map[int][]byte{}
	s.photoType = map[int]string{}
	s.tasks = map[int]*Task{}
	s.nextID = 1
	s.nextTaskID = 1
}

func (s *store) review(id int, status, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	if !ok || f.Status != "待簽核" {
		return false
	}
	f.Status = status
	f.RejectReason = reason
	f.ReviewedAt = time.Now()
	return true
}

func (s *store) addTask(t *Task) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.ID = s.nextTaskID
	s.nextTaskID++
	s.tasks[t.ID] = t
	return t
}

func (s *store) getTask(id int) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	return t, ok
}

func (s *store) linkTask(taskID, formID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[taskID]; ok {
		t.FormID = formID
	}
}

// taskViews returns tasks (filtered by engineer when non-empty, 未填單在前)
// plus the distinct engineer names for the filter chips.
func (s *store) taskViews(engineer string) (views []TaskView, engineers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range s.tasks {
		if !seen[t.Engineer] {
			seen[t.Engineer] = true
			engineers = append(engineers, t.Engineer)
		}
		if engineer != "" && t.Engineer != engineer {
			continue
		}
		status := "未填單"
		if f, ok := s.forms[t.FormID]; ok {
			status = f.Status
		}
		views = append(views, TaskView{Task: t, Status: status})
	}
	sort.Strings(engineers)
	sort.Slice(views, func(i, j int) bool {
		ti, tj := views[i].Status == "未填單", views[j].Status == "未填單"
		if ti != tj {
			return ti
		}
		return views[i].ID < views[j].ID
	})
	return views, engineers
}

func seed(s *store) {
	items := func(oks ...bool) []CheckItem {
		var out []CheckItem
		for i, name := range checkItemNames {
			out = append(out, CheckItem{Name: name, OK: oks[i]})
		}
		return out
	}
	t1 := s.addTask(&Task{Customer: "富宇建設", Site: "桃園青埔物流中心 新建工程", Panel: "P1-LP-3", Engineer: "陳志明"})
	t2 := s.addTask(&Task{Customer: "台茂精密", Site: "中壢工業區 廠務改善案", Panel: "MCC-2 盤", Engineer: "林大偉"})
	s.addTask(&Task{Customer: "桃園捷運公司", Site: "A19 站機電年度保養", Panel: "HV-1 高壓盤", Engineer: "陳志明"})
	t4 := s.addTask(&Task{Customer: "幸福水泥", Site: "楊梅廠年度檢測", Panel: "ATS-1", Engineer: "陳志明"})
	s.addTask(&Task{Customer: "大江購物中心", Site: "消防設備複檢", Panel: "FP-3 消防盤", Engineer: "林大偉"})

	done := s.add(&Form{
		TaskID:   t1.ID,
		Customer: "富宇建設", Site: "桃園青埔物流中心 新建工程", Panel: "P1-LP-3",
		Engineer: "陳志明",
		Items:    items(true, true, true, true, true, true),
		Note:     "全數合格，已拍照存查。",
		Status:   "待簽核", CreatedAt: time.Now().Add(-26 * time.Hour),
	}, nil, "")
	s.review(done.ID, "已核准", "")
	s.linkTask(t1.ID, done.ID)

	waiting := s.add(&Form{
		TaskID:   t2.ID,
		Customer: "台茂精密", Site: "中壢工業區 廠務改善案", Panel: "MCC-2 盤",
		Engineer: "林大偉",
		Items:    items(true, true, false, true, true, true),
		Note:     "匯流排 B 相接點有過熱變色，建議停機檢修後複測。",
		Status:   "待簽核", CreatedAt: time.Now().Add(-40 * time.Minute),
	}, nil, "")
	s.linkTask(t2.ID, waiting.ID)

	rejected := s.add(&Form{
		TaskID:   t4.ID,
		Customer: "幸福水泥", Site: "楊梅廠年度檢測", Panel: "ATS-1",
		Engineer: "陳志明",
		Items:    items(true, false, true, true, true, true),
		Note:     "接地阻抗量測值偏高，待複測。",
		Status:   "待簽核", CreatedAt: time.Now().Add(-3 * time.Hour),
	}, nil, "")
	s.review(rejected.ID, "已退回", "接地阻抗超標，請重新檢測並附上量測數據照片")
	s.linkTask(t4.ID, rejected.ID)
}

var tmpl *template.Template

func render(w http.ResponseWriter, name string, data any) {
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func main() {
	tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))
	s := newStore()
	seed(s)

	// 公開示意站防呆：每小時清掉訪客留下的資料，回到種子狀態
	go func() {
		for range time.Tick(time.Hour) {
			s.clear()
			seed(s)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "index.html", nil)
	})

	mux.HandleFunc("GET /qr", func(w http.ResponseWriter, r *http.Request) {
		render(w, "qr.html", nil)
	})

	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		selected := r.URL.Query().Get("engineer")
		views, engineers := s.taskViews(selected)
		render(w, "tasks.html", map[string]any{
			"Views": views, "Engineers": engineers, "Selected": selected,
		})
	})

	mux.HandleFunc("GET /new", func(w http.ResponseWriter, r *http.Request) {
		var task *Task
		var prevRejectReason string
		if id, err := strconv.Atoi(r.URL.Query().Get("task")); err == nil {
			if task, _ = s.getTask(id); task != nil {
				if f, ok := s.get(task.FormID); ok && f.Status == "已退回" {
					prevRejectReason = f.RejectReason
				}
			}
		}
		render(w, "new.html", map[string]any{
			"Items": checkItemNames, "Task": task, "PrevRejectReason": prevRejectReason,
		})
	})

	mux.HandleFunc("POST /forms", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "表單解析失敗", http.StatusBadRequest)
			return
		}
		checked := map[string]bool{}
		for _, v := range r.Form["item"] {
			checked[v] = true
		}
		var items []CheckItem
		for _, name := range checkItemNames {
			items = append(items, CheckItem{Name: name, OK: checked[name]})
		}
		var photo []byte
		var photoType string
		if file, header, err := r.FormFile("photo"); err == nil {
			photo, _ = io.ReadAll(file)
			file.Close()
			photoType = header.Header.Get("Content-Type")
		}
		taskID, _ := strconv.Atoi(r.FormValue("task")) // 0 表示臨時填單，非派工清單觸發
		f := s.add(&Form{
			TaskID:    taskID,
			Customer:  r.FormValue("customer"),
			Site:      r.FormValue("site"),
			Panel:     r.FormValue("panel"),
			Engineer:  r.FormValue("engineer"),
			Items:     items,
			Note:      r.FormValue("note"),
			Signature: parseSignature(r.FormValue("signature")),
			Status:    "待簽核",
			CreatedAt: time.Now(),
		}, photo, photoType)
		if taskID != 0 {
			s.linkTask(taskID, f.ID)
		}
		http.Redirect(w, r, fmt.Sprintf("/forms/%d?from=tasks", f.ID), http.StatusSeeOther)
	})

	mux.HandleFunc("GET /forms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f, ok := s.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		back, backLabel := "/admin", "簽核後台"
		if r.URL.Query().Get("from") == "tasks" {
			back, backLabel = "/tasks", "待簽客戶"
		}
		history := s.formsForTask(f.TaskID)
		if len(history) < 2 {
			history = nil
		}
		render(w, "detail.html", map[string]any{
			"Form": f, "Back": back, "BackLabel": backLabel, "History": history,
		})
	})

	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		pending, processed := s.list()
		render(w, "admin.html", map[string]any{
			"Pending": pending, "Processed": processed, "Stats": s.stats(),
			"NotifiedEngineer": r.URL.Query().Get("notified"),
			"NotifiedAction":   r.URL.Query().Get("action"),
		})
	})

	review := func(status string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.Atoi(r.PathValue("id"))
			s.review(id, status, r.FormValue("reason"))
			engineer := ""
			if f, ok := s.get(id); ok {
				engineer = f.Engineer
			}
			q := url.Values{"notified": {engineer}, "action": {status}}
			http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
		}
	}
	mux.HandleFunc("POST /forms/{id}/approve", review("已核准"))
	mux.HandleFunc("POST /forms/{id}/reject", review("已退回"))

	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		s.addTask(&Task{
			Customer: r.FormValue("customer"),
			Site:     r.FormValue("site"),
			Panel:    r.FormValue("panel"),
			Engineer: r.FormValue("engineer"),
		})
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /photos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		data, ctype := s.photo(id)
		if len(data) == 0 {
			http.NotFound(w, r)
			return
		}
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.Write(data)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Println("行動簽核 DEMO 已啟動")
	fmt.Printf("  電腦開： http://localhost%s\n", addr)
	if ip := lanIP(); ip != "" {
		fmt.Printf("  手機開： http://%s%s （需同一 Wi-Fi）\n", ip, addr)
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}

// lanIP returns this machine's LAN IPv4, for opening the demo from a phone.
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}
