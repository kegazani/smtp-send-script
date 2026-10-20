package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const appVersion = "1.1.5"

type runningState struct {
	Version       string `json:"version"`
	TestRecipient string `json:"testRecipient"`
	Sending       bool   `json:"sending"`
	Generating    bool   `json:"generating"`
}

// This client talks only to the authenticated server recorded by this profile.
// It never uses an HTTP proxy or follows a redirect to another destination.
func lifecycleClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func instanceRequest(client *http.Client, old instance, method, path string) (*http.Response, error) {
	origin := "http://127.0.0.1:" + strconv.Itoa(old.Port)
	req, err := http.NewRequest(method, origin+path, bytes.NewBufferString("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "maildesk", Value: old.Session})
	return client.Do(req)
}

func runningInstance(client *http.Client, dir string) (instance, *runningState, error) {
	var old instance
	if err := readJSON(filepath.Join(dir, "instance.json"), &old); os.IsNotExist(err) {
		return old, nil, nil
	} else if err != nil {
		return old, nil, fmt.Errorf("не удалось прочитать сведения о работающем MailDesk: %w", err)
	}
	if old.Port < 1 || old.Port > 65535 || old.Session == "" {
		return old, nil, errors.New("неверные сведения о работающем MailDesk")
	}
	res, err := instanceRequest(client, old, "GET", "/api/state")
	if errors.Is(err, syscall.ECONNREFUSED) {
		return old, nil, nil // A stale record left by a stopped server.
	}
	if err != nil {
		return old, nil, fmt.Errorf("работающий MailDesk не отвечает; завершите его перед обновлением: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404 {
		return old, nil, nil // This port no longer belongs to this instance.
	}
	if res.StatusCode != 200 {
		return old, nil, fmt.Errorf("MailDesk вернул HTTP %d", res.StatusCode)
	}
	var state runningState
	if err := json.NewDecoder(io.LimitReader(res.Body, maxProjectJSON)).Decode(&state); err != nil {
		return old, nil, fmt.Errorf("не удалось проверить состояние MailDesk: %w", err)
	}
	return old, &state, nil
}

func prepareUpdate(dir string) error {
	client := lifecycleClient()
	defer client.CloseIdleConnections()
	old, state, err := runningInstance(client, dir)
	if err != nil || state == nil {
		return err
	}
	if state.Sending || state.Generating {
		return errors.New("идёт отправка или генерация; дождитесь завершения либо остановите очередь перед обновлением")
	}
	res, err := instanceRequest(client, old, "POST", "/api/quit")
	if err != nil {
		return fmt.Errorf("не удалось завершить предыдущую версию: %w", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("предыдущая версия отказалась завершать работу (HTTP %d); дождитесь окончания операции", res.StatusCode)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, state, err = runningInstance(client, dir)
		if err != nil {
			return err
		}
		if state == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("предыдущая версия ещё работает; установка отменена")
}

func waitReady(dir, testRecipient string) error {
	client := lifecycleClient()
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		_, state, err := runningInstance(client, dir)
		if err != nil {
			return err
		}
		if state != nil {
			if state.Version != appVersion || state.TestRecipient != testRecipient {
				return errors.New("ответила другая версия или другой режим MailDesk; установка не завершена")
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("новая версия не запустила локальный сервер; проверьте показанный установщиком журнал")
}
