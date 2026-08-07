package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// panelSize 问 EInkRelay 设备可见面板几何。画布与面板一致时
// fit=contain 是 1:1 显示，不发生任何重采样。
func panelSize(ctx context.Context, host, token string) (int, int, error) {
	var status struct {
		Screen *struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"screen"`
	}
	err := getJSON(ctx, "http://"+host+"/v1/status", map[string]string{
		"Authorization": "Bearer " + token,
	}, &status)
	if err != nil {
		return 0, 0, err
	}
	if status.Screen == nil || status.Screen.Width <= 0 || status.Screen.Height <= 0 {
		return 0, 0, fmt.Errorf("设备未能探测面板几何")
	}
	return status.Screen.Width, status.Screen.Height, nil
}

// pushMarkdown 把 Markdown 推给 EInkRelay，由设备端排版显示。
func pushMarkdown(ctx context.Context, host, token, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("http://%s/v1/display/markdown?font_size=%d", host, textFontSize),
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("推送失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// push 把 PNG 推给 EInkRelay 显示。服务不排队：占屏中会收到
// 409 display_busy，调用方应稍后重试而不是并发重发。
func push(ctx context.Context, host, token string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://"+host+"/v1/display/image?fit=contain", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "image/png")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("推送失败: HTTP %d", resp.StatusCode)
	}
	return nil
}
