package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

//go:embed web/index.html web/login.html
var webFS embed.FS

var (
	authPassword  string
	sessions      = make(map[string]time.Time)
	sessionsMux   sync.RWMutex
	loginAttempts = make(map[string][]time.Time)
	attemptsMux   sync.RWMutex
)

const sessionCookieName = "logtailer_session"
const sessionDuration = 24 * time.Hour
const maxLoginAttempts = 20
const loginWindowDuration = 1 * time.Minute

func generateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Printf("生成随机数失败: %v", err)
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

func isValidSession(sessionID string) bool {
	if authPassword == "" {
		return true
	}
	sessionsMux.RLock()
	defer sessionsMux.RUnlock()
	expiry, exists := sessions[sessionID]
	return exists && time.Now().Before(expiry)
}

func createSession() string {
	sessionID := generateSessionID()
	sessionsMux.Lock()
	sessions[sessionID] = time.Now().Add(sessionDuration)
	sessionsMux.Unlock()
	return sessionID
}

func cleanupExpiredSessions() {
	ticker := time.NewTicker(time.Hour)
	for range ticker.C {
		now := time.Now()
		sessionsMux.Lock()
		for id, expiry := range sessions {
			if now.After(expiry) {
				delete(sessions, id)
			}
		}
		sessionsMux.Unlock()

		attemptsMux.Lock()
		for ip, attempts := range loginAttempts {
			var valid []time.Time
			for _, t := range attempts {
				if now.Sub(t) < loginWindowDuration {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(loginAttempts, ip)
			} else {
				loginAttempts[ip] = valid
			}
		}
		attemptsMux.Unlock()
	}
}

func isRateLimited(ip string) bool {
	attemptsMux.RLock()
	defer attemptsMux.RUnlock()

	attempts, exists := loginAttempts[ip]
	if !exists {
		return false
	}

	now := time.Now()
	count := 0
	for _, t := range attempts {
		if now.Sub(t) < loginWindowDuration {
			count++
		}
	}
	return count >= maxLoginAttempts
}

func recordLoginAttempt(ip string) {
	attemptsMux.Lock()
	defer attemptsMux.Unlock()
	loginAttempts[ip] = append(loginAttempts[ip], time.Now())
}

func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authPassword == "" {
			next(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !isValidSession(cookie.Value) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	return r.RemoteAddr
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if authPassword == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if r.Method == "GET" {
		content, err := webFS.ReadFile("web/login.html")
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(content)
		return
	}

	if r.Method == "POST" {
		clientIP := getClientIP(r)

		if isRateLimited(clientIP) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"success": false, "message": "尝试次数过多，请稍后再试"}`))
			return
		}

		r.ParseForm()
		password := r.FormValue("password")

		if secureCompare(password, authPassword) {
			sessionID := createSession()
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Value:    sessionID,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(sessionDuration.Seconds()),
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success": true}`))
		} else {
			recordLoginAttempt(clientIP)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"success": false, "message": "密码错误"}`))
		}
		return
	}

	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		host := r.Host
		return origin == "http://"+host || origin == "https://"+host
	},
}

type LogTailer struct {
	filename   string
	clients    map[*websocket.Conn]bool
	clientsMux sync.RWMutex
	watcher    *fsnotify.Watcher
	lastSize   int64
	lastLines  []string
	linesMux   sync.RWMutex
	maxLines   int
}

type InitMessage struct {
	Type     string   `json:"type"`
	Filename string   `json:"filename"`
	Filesize int64    `json:"filesize"`
	Lines    []string `json:"lines"`
}

type LogMessage struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type FileSizeMessage struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type ClientMessage struct {
	Type  string `json:"type"`
	Lines int    `json:"lines"`
}

func NewLogTailer(filename string, maxLines int) (*LogTailer, error) {
	absPath, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("获取绝对路径失败: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建文件监听器失败: %w", err)
	}

	lt := &LogTailer{
		filename: absPath,
		clients:  make(map[*websocket.Conn]bool),
		watcher:  watcher,
		maxLines: maxLines,
	}

	return lt, nil
}

func (lt *LogTailer) Start() error {
	if _, err := os.Stat(lt.filename); os.IsNotExist(err) {
		return fmt.Errorf("文件不存在: %s", lt.filename)
	}

	lines, size, err := lt.readLastLines()
	if err != nil {
		log.Printf("读取历史日志失败: %v", err)
	} else {
		lt.linesMux.Lock()
		lt.lastLines = lines
		lt.lastSize = size
		lt.linesMux.Unlock()
	}

	if err := lt.watcher.Add(lt.filename); err != nil {
		dir := filepath.Dir(lt.filename)
		if err := lt.watcher.Add(dir); err != nil {
			return fmt.Errorf("监听文件失败: %w", err)
		}
	}

	go lt.watchLoop()

	return nil
}

func (lt *LogTailer) watchLoop() {
	for {
		select {
		case event, ok := <-lt.watcher.Events:
			if !ok {
				return
			}

			absEventPath, _ := filepath.Abs(event.Name)
			if absEventPath != lt.filename {
				continue
			}

			if event.Op&fsnotify.Write == fsnotify.Write {
				lt.handleFileChange()
			} else if event.Op&fsnotify.Create == fsnotify.Create {
				lt.handleFileChange()
			} else if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
				lt.linesMux.Lock()
				lt.lastSize = 0
				lt.linesMux.Unlock()
			}

		case err, ok := <-lt.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("监听错误: %v", err)
		}
	}
}

func (lt *LogTailer) handleFileChange() {
	file, err := os.Open(lt.filename)
	if err != nil {
		log.Printf("打开文件失败: %v", err)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		log.Printf("获取文件信息失败: %v", err)
		return
	}

	currentSize := stat.Size()

	lt.linesMux.Lock()
	lastSize := lt.lastSize
	lt.linesMux.Unlock()

	if currentSize < lastSize {
		lt.linesMux.Lock()
		lt.lastSize = 0
		lastSize = 0
		lt.linesMux.Unlock()
	}

	if currentSize > lastSize {
		lt.broadcastFileSize(currentSize)

		newContent, err := lt.readNewContent(file, lastSize, currentSize)
		if err != nil {
			log.Printf("读取新内容失败: %v", err)
			return
		}

		lt.linesMux.Lock()
		lt.lastSize = currentSize
		lt.linesMux.Unlock()

		lines := splitLines(newContent)
		for _, line := range lines {
			if line != "" {
				lt.broadcastLine(line)
			}
		}
	}
}

func (lt *LogTailer) readNewContent(file *os.File, start, end int64) (string, error) {
	size := end - start
	if size > 10*1024*1024 {
		start = end - 10*1024*1024
		size = 10 * 1024 * 1024
	}

	_, err := file.Seek(start, io.SeekStart)
	if err != nil {
		return "", err
	}

	buf := make([]byte, size)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}

	content := buf[:n]
	return convertToUTF8(content), nil
}

func (lt *LogTailer) readLastLines() ([]string, int64, error) {
	return lt.readLastNLines(lt.maxLines)
}

func (lt *LogTailer) readLastNLines(maxLines int) ([]string, int64, error) {
	file, err := os.Open(lt.filename)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}

	fileSize := stat.Size()
	if fileSize == 0 {
		return []string{}, 0, nil
	}

	chunkSize := int64(64 * 1024)
	var allContent []byte
	position := fileSize
	lineCount := 0

	for position > 0 && lineCount <= maxLines {
		readSize := chunkSize
		if position < readSize {
			readSize = position
		}
		position -= readSize

		_, err := file.Seek(position, io.SeekStart)
		if err != nil {
			return nil, 0, err
		}

		buf := make([]byte, readSize)
		n, err := io.ReadFull(file, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, 0, err
		}

		allContent = append(buf[:n], allContent...)
		lineCount = bytes.Count(allContent, []byte{'\n'})
	}

	content := convertToUTF8(allContent)
	lines := splitLines(content)

	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}

	return result, fileSize, nil
}

func splitLines(content string) []string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader([]byte(content)))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func convertToUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}

	reader := transform.NewReader(bytes.NewReader(data), simplifiedchinese.GBK.NewDecoder())
	result, err := io.ReadAll(reader)
	if err != nil {
		return string(data)
	}
	return string(result)
}

func (lt *LogTailer) broadcastLine(line string) {
	msg := LogMessage{
		Type:    "log",
		Content: line,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("JSON编码失败: %v", err)
		return
	}

	lt.clientsMux.RLock()
	defer lt.clientsMux.RUnlock()

	for client := range lt.clients {
		err := client.WriteMessage(websocket.TextMessage, data)
		if err != nil {
			log.Printf("发送消息失败: %v", err)
		}
	}
}

func (lt *LogTailer) broadcastFileSize(size int64) {
	msg := FileSizeMessage{
		Type: "filesize",
		Size: size,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	lt.clientsMux.RLock()
	defer lt.clientsMux.RUnlock()

	for client := range lt.clients {
		client.WriteMessage(websocket.TextMessage, data)
	}
}

func (lt *LogTailer) addClient(conn *websocket.Conn) {
	lt.clientsMux.Lock()
	lt.clients[conn] = true
	lt.clientsMux.Unlock()

	lt.linesMux.RLock()
	lines := make([]string, len(lt.lastLines))
	copy(lines, lt.lastLines)
	lastSize := lt.lastSize
	lt.linesMux.RUnlock()

	initMsg := InitMessage{
		Type:     "init",
		Filename: filepath.Base(lt.filename),
		Filesize: lastSize,
		Lines:    lines,
	}

	data, err := json.Marshal(initMsg)
	if err != nil {
		log.Printf("JSON编码失败: %v", err)
		return
	}

	conn.WriteMessage(websocket.TextMessage, data)
}

func (lt *LogTailer) removeClient(conn *websocket.Conn) {
	lt.clientsMux.Lock()
	delete(lt.clients, conn)
	lt.clientsMux.Unlock()
}

func (lt *LogTailer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}
	defer conn.Close()

	lt.addClient(conn)
	defer lt.removeClient(conn)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var clientMsg ClientMessage
		if err := json.Unmarshal(message, &clientMsg); err != nil {
			continue
		}

		if clientMsg.Type == "reload" {
			lines := clientMsg.Lines
			if lines < 1 {
				lines = 1
			}
			if lines > 10000 {
				lines = 10000
			}

			historyLines, fileSize, err := lt.readLastNLines(lines)
			if err != nil {
				log.Printf("重新加载日志失败: %v", err)
				continue
			}

			initMsg := InitMessage{
				Type:     "init",
				Filename: filepath.Base(lt.filename),
				Filesize: fileSize,
				Lines:    historyLines,
			}

			data, err := json.Marshal(initMsg)
			if err != nil {
				log.Printf("JSON编码失败: %v", err)
				continue
			}

			conn.WriteMessage(websocket.TextMessage, data)
		}
	}
}

func (lt *LogTailer) handleIndex(w http.ResponseWriter, r *http.Request) {
	content, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func reorderArgs(args []string) []string {
	var flags []string
	var positional []string

	i := 0
	for i < len(args) {
		arg := args[i]
		if len(arg) > 0 && arg[0] == '-' {
			flags = append(flags, arg)
			if i+1 < len(args) && len(args[i+1]) > 0 && args[i+1][0] != '-' {
				if arg == "-port" || arg == "--port" || arg == "-host" || arg == "--host" || arg == "-lines" || arg == "--lines" || arg == "-auth" || arg == "--auth" {
					i++
					flags = append(flags, args[i])
				}
			}
		} else {
			positional = append(positional, arg)
		}
		i++
	}

	return append(flags, positional...)
}

func main() {
	port := flag.Int("port", 8080, "HTTP服务端口")
	host := flag.String("host", "0.0.0.0", "监听地址")
	lines := flag.Int("lines", 100, "显示的历史日志行数")
	auth := flag.String("auth", "", "访问密码（留空则无需认证）")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [选项] <日志文件>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "选项:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  %s app.log --port 8080\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s /var/log/syslog --host 127.0.0.1 --port 9000 --lines 200\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s app.log --auth mypassword\n", os.Args[0])
	}

	args := reorderArgs(os.Args[1:])
	flag.CommandLine.Parse(args)

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	filename := flag.Arg(0)
	authPassword = *auth

	if authPassword != "" {
		go cleanupExpiredSessions()
	}

	tailer, err := NewLogTailer(filename, *lines)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	if err := tailer.Start(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	http.HandleFunc("/", authMiddleware(tailer.handleIndex))
	http.HandleFunc("/ws", authMiddleware(tailer.handleWebSocket))
	http.HandleFunc("/login", handleLogin)

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("LogTailer 启动成功")
	log.Printf("  文件: %s", tailer.filename)
	log.Printf("  监听: %s", addr)
	log.Printf("  历史行数: %d", *lines)
	if authPassword != "" {
		log.Printf("  认证: 已启用")
	} else {
		log.Printf("  认证: 未启用")
	}
	if *host == "0.0.0.0" {
		log.Printf("  访问地址: http://localhost:%d", *port)
	} else {
		log.Printf("  访问地址: http://%s", addr)
	}

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("HTTP服务启动失败: %v", err)
	}
}
