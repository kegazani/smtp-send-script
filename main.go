package main

import (
	"bufio"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

func loadEnv(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), "\"'")
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
	return s.Err()
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func (a *App) handler() http.Handler {
	routes := a.routes()
	sub, _ := fs.Sub(assets, "web")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; form-action 'self'")
		if "http://"+r.Host != a.origin {
			http.Error(w, "Недопустимый адрес", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != a.origin {
			http.Error(w, "Недопустимый источник", 403)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Get("session") != "" {
			if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("session")), []byte(a.session)) != 1 {
				http.Error(w, "Нет доступа", 403)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "maildesk", Value: a.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", 303)
			return
		}
		cookie, err := r.Cookie("maildesk")
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(a.session)) != 1 {
			http.Error(w, "Откройте MailDesk через приложение или адрес, показанный в его окне.", 401)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			routes.ServeHTTP(w, r)
		} else {
			files.ServeHTTP(w, r)
		}
	})
}

type instance struct {
	Port    int    `json:"port"`
	Session string `json:"session"`
}

func main() {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	dir := flag.String("data-dir", filepath.Join(base, "MailDesk"), "папка локальных данных")
	port := flag.Int("port", 0, "порт локального интерфейса (0 — свободный)")
	envFile := flag.String("env-file", "", "локальный файл переменных")
	seed := flag.String("import", "", "CSV для первоначального импорта в пустой проект")
	noBrowser := flag.Bool("no-browser", false, "не открывать браузер")
	testRecipient := flag.String("test-recipient", "", "разрешить отправку только на этот тестовый адрес")
	flag.Parse()
	// Optional personal distributions keep credentials beside, never inside, the executable.
	if executable, e := os.Executable(); e == nil {
		adjacent := filepath.Dir(executable)
		if runtime.GOOS == "darwin" && filepath.Base(adjacent) == "MacOS" && filepath.Base(filepath.Dir(adjacent)) == "Contents" {
			adjacent = filepath.Clean(filepath.Join(adjacent, "../../.."))
		}
		if *envFile == "" {
			p := filepath.Join(adjacent, "maildesk.env")
			if _, e := os.Stat(p); e == nil {
				*envFile = p
			}
		}
		if *seed == "" {
			p := filepath.Join(adjacent, "companies.csv")
			if _, e := os.Stat(p); e == nil {
				*seed = p
			}
		}
	}
	if err = loadEnv(*envFile); err != nil {
		fatal(err)
	}
	if *testRecipient != "" && !validEmail(*testRecipient) {
		fatal(fmt.Errorf("неверный тестовый адрес"))
	}
	if err = os.MkdirAll(*dir, 0700); err != nil {
		fatal(err)
	}
	// A local file binds one data directory to one live instance. It stores no SMTP credentials.
	instancePath := filepath.Join(*dir, "instance.json")
	if b, e := os.ReadFile(instancePath); e == nil {
		var old instance
		if json.Unmarshal(b, &old) == nil && old.Port > 0 && old.Port < 65536 {
			url := "http://127.0.0.1:" + strconv.Itoa(old.Port)
			req, _ := http.NewRequest("GET", url+"/api/state", nil)
			req.AddCookie(&http.Cookie{Name: "maildesk", Value: old.Session})
			client := &http.Client{Timeout: time.Second}
			res, e := client.Do(req)
			if e == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					if !*noBrowser {
						openBrowser(url + "/?session=" + old.Session)
					}
					fmt.Println("MailDesk уже запущен.")
					return
				}
			}
		}
	}
	// Lock startup to prevent two processes loading the same project simultaneously.
	lockPath := filepath.Join(*dir, "startup.lock")
	if info, e := os.Stat(lockPath); e == nil && time.Since(info.ModTime()) > 30*time.Second {
		_ = os.Remove(lockPath)
	}
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		fatal(fmt.Errorf("папка данных уже используется; если приложение аварийно завершилось, удалите startup.lock в %s", *dir))
	}
	lock.Close()
	defer os.Remove(lockPath)
	a, err := openApp(*dir)
	if err != nil {
		fatal(err)
	}
	a.testRecipient = *testRecipient
	if *seed != "" && len(a.w.Leads) == 0 {
		f, e := os.Open(*seed)
		if e != nil {
			fatal(e)
		}
		leads, e := csvLeads(f)
		f.Close()
		if e != nil {
			fatal(e)
		}
		a.merge(leads, nil)
		if e = a.save(); e != nil {
			fatal(e)
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fatal(err)
	}
	a.origin = "http://" + listener.Addr().String()
	if err = atomicJSON(instancePath, instance{Port: listener.Addr().(*net.TCPAddr).Port, Session: a.session}); err != nil {
		fatal(err)
	}
	defer os.Remove(instancePath)
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	a.shutdown = func() { _ = server.Close() }
	_ = os.Remove(lockPath)
	url := a.origin + "/?session=" + a.session
	fmt.Println("MailDesk запущен. Закрытие этого окна останавливает приложение.")
	fmt.Println("Откройте в браузере:", url)
	if *testRecipient != "" {
		fmt.Println("Тестовый режим: отправка только на", *testRecipient)
	}
	if !*noBrowser {
		openBrowser(url)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; _ = server.Close() }()
	if e := server.Serve(listener); e != nil && e != http.ErrServerClosed {
		fatal(e)
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "MailDesk:", err); os.Exit(1) }
