// Command quota-dashboard 汇总本机八家 AI 编程服务的额度使用情况，
// 渲染成一张适合电子墨水屏的灰度 PNG，并推送给 EInkRelay。
//
// 数据源（全部在本机实测可用）：
//
//	Claude      Keychain「Claude Code-credentials」→ api.anthropic.com/api/oauth/usage
//	Codex       ~/.codex/auth.json                → chatgpt.com/backend-api/wham/usage
//	Grok        ~/.grok/auth.json                 → cli-chat-proxy.grok.com/v1/billing
//	Kimi        ~/.kimi-code/credentials/…        → api.kimi.com/coding/v1/usages
//	GLM         环境变量 ZHIPUAI_API_KEY           → open.bigmodel.cn/api/monitor/usage/quota/limit
//	MiniMax     环境变量 MINIMAX_API_KEY           → api.minimaxi.com/v1/api/openplatform/coding_plan/remains
//	Antigravity Keychain service「gemini」account「antigravity」→ daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels
//	DeepSeek    环境变量 DEEPSEEK_API_KEY          → api.deepseek.com/user/balance
//
// 用法：
//
//	DASHBOARD_FONT=/path/to/NotoSansCJKsc-Regular.otf \
//	EINKRELAY_HOST=192.168.15.244:8080 EINKRELAY_TOKEN=... \
//	go run .                # 查询八家额度 → 渲染 → 推送到 Kindle
//
//	go run . -o out.png     # 只渲染到本地文件，不推送
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Window 是一个计费窗口的用量。
type Window struct {
	Label         string    // 「5小时」「本周」「本月」或模型族名
	UsedPercent   float64   // 0–100；-1 表示未知/不适用
	ResetAt       time.Time // 零值表示未知
	Note          string    // 非百分比信息（如账户余额），设置后替代百分比显示
	Used          float64   // 已用额度（绝对量）；<0 表示服务端不提供
	Total         float64   // 总额度（绝对量）；<=0 表示服务端不提供
	ConstrainedBy string    // 非空表示本窗口被更长周期的额度覆盖（值为该周期标签）
}

// ServiceUsage 是一家服务的查询结果。Err 非空时 Windows 无效。
type ServiceUsage struct {
	Name    string
	Plan    string
	Windows []Window
	Err     error
}

const httpTimeout = 15 * time.Second

func getJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	return doJSON(ctx, http.MethodGet, url, headers, nil, out)
}

func postJSON(ctx context.Context, url string, headers map[string]string, body string, out any) error {
	return doJSON(ctx, http.MethodPost, url, headers, strings.NewReader(body), out)
}

func doJSON(ctx context.Context, method, url string, headers map[string]string, body io.Reader, out any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if seeker, ok := body.(io.Seeker); ok {
				seeker.Seek(0, io.SeekStart)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		lastErr = doJSONOnce(ctx, method, url, headers, body, out)
		if lastErr == nil {
			return nil
		}
		// HTTP 状态码错误（401/403/404…）重试无意义，只重试网络层错误
		var httpErr *httpStatusError
		if errors.As(lastErr, &httpErr) {
			return lastErr
		}
	}
	return lastErr
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func doJSONOnce(ctx context.Context, method, url string, headers map[string]string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{code: resp.StatusCode}
	}
	return json.Unmarshal(respBody, out)
}

func homePath(elem ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, elem...)...)
}

// parseResetTime 兼容 RFC3339 字符串与毫秒/秒级 Unix 时间戳。
func parseResetTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return epochToTime(n)
		}
	case float64:
		return epochToTime(int64(t))
	}
	return time.Time{}
}

func epochToTime(n int64) time.Time {
	if n > 1e12 { // 毫秒
		return time.UnixMilli(n)
	}
	if n > 1e9 { // 秒
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// windowLabel 按窗口长度给一个中文标签。
func windowLabel(seconds int64) string {
	switch {
	case seconds <= 6*3600:
		return "5小时"
	case seconds <= 8*24*3600:
		return "本周"
	default:
		return "本月"
	}
}

// ─── Claude ───────────────────────────────────────────────────────────────

// claudeAccessToken 从 macOS Keychain 取 Claude Code 的 OAuth token；
// Linux 上回退到 ~/.claude/.credentials.json。
func claudeAccessToken() (string, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err == nil {
			var creds struct {
				Oauth struct {
					AccessToken string `json:"accessToken"`
				} `json:"claudeAiOauth"`
			}
			if json.Unmarshal(out, &creds) == nil && creds.Oauth.AccessToken != "" {
				return creds.Oauth.AccessToken, nil
			}
		}
	}
	raw, err := os.ReadFile(homePath(".claude", ".credentials.json"))
	if err != nil {
		return "", errors.New("Keychain 与 ~/.claude/.credentials.json 均不可用")
	}
	var creds struct {
		Oauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil || creds.Oauth.AccessToken == "" {
		return "", errors.New("无法解析 Claude 凭据")
	}
	return creds.Oauth.AccessToken, nil
}

func probeClaude(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Claude"}
	token, err := claudeAccessToken()
	if err != nil {
		s.Err = err
		return s
	}
	var resp struct {
		FiveHour struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    any     `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    any     `json:"resets_at"`
		} `json:"seven_day"`
	}
	err = getJSON(ctx, "https://api.anthropic.com/api/oauth/usage", map[string]string{
		"Authorization":   "Bearer " + token,
		"anthropic-beta":  "oauth-2025-04-20",
		"accept":          "application/json",
		"user-agent":      "quota-dashboard",
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	s.Plan = "subscription"
	s.Windows = []Window{
		{Label: "5小时", UsedPercent: resp.FiveHour.Utilization, ResetAt: parseResetTime(resp.FiveHour.ResetsAt)},
		{Label: "本周", UsedPercent: resp.SevenDay.Utilization, ResetAt: parseResetTime(resp.SevenDay.ResetsAt)},
	}
	return s
}

// ─── Codex ────────────────────────────────────────────────────────────────

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func probeCodex(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Codex"}
	raw, err := os.ReadFile(homePath(".codex", "auth.json"))
	if err != nil {
		s.Err = errors.New("~/.codex/auth.json 不可读")
		return s
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil || auth.Tokens.AccessToken == "" {
		s.Err = errors.New("无法解析 Codex 凭据")
		return s
	}
	var resp struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			PrimaryWindow   *codexWindow `json:"primary_window"`
			SecondaryWindow *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	err = getJSON(ctx, "https://chatgpt.com/backend-api/wham/usage", map[string]string{
		"Authorization":      "Bearer " + auth.Tokens.AccessToken,
		"ChatGPT-Account-Id": auth.Tokens.AccountID,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	s.Plan = resp.PlanType
	for _, w := range []*codexWindow{resp.RateLimit.SecondaryWindow, resp.RateLimit.PrimaryWindow} {
		if w == nil {
			continue
		}
		s.Windows = append(s.Windows, Window{
			Label:       windowLabel(w.LimitWindowSeconds),
			UsedPercent: w.UsedPercent,
			ResetAt:     epochToTime(w.ResetAt),
		})
	}
	if len(s.Windows) == 0 {
		s.Err = errors.New("响应中没有限流窗口")
	}
	return s
}

// ─── Grok ─────────────────────────────────────────────────────────────────

func probeGrok(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Grok"}
	raw, err := os.ReadFile(homePath(".grok", "auth.json"))
	if err != nil {
		s.Err = errors.New("~/.grok/auth.json 不可读")
		return s
	}
	var entries map[string]struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		s.Err = errors.New("无法解析 Grok 凭据")
		return s
	}
	var key string
	for id, e := range entries {
		if strings.HasPrefix(id, "https://auth.x.ai::") && e.Key != "" {
			key = e.Key
			break
		}
	}
	if key == "" {
		s.Err = errors.New("~/.grok/auth.json 中没有 auth.x.ai 凭据")
		return s
	}
	var resp struct {
		Config struct {
			MonthlyLimit struct {
				Val float64 `json:"val"`
			} `json:"monthlyLimit"`
			Used struct {
				Val float64 `json:"val"`
			} `json:"used"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"config"`
	}
	err = getJSON(ctx, "https://cli-chat-proxy.grok.com/v1/billing", map[string]string{
		"Authorization": "Bearer " + key,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	used := -1.0
	if resp.Config.MonthlyLimit.Val > 0 {
		used = resp.Config.Used.Val / resp.Config.MonthlyLimit.Val * 100
		s.Plan = fmt.Sprintf("%.0f/%.0f credits", resp.Config.Used.Val, resp.Config.MonthlyLimit.Val)
	}
	s.Windows = []Window{{
		Label:       "本月",
		UsedPercent: used,
		ResetAt:     parseResetTime(resp.Config.BillingPeriodEnd),
		Used:        resp.Config.Used.Val,
		Total:       resp.Config.MonthlyLimit.Val,
	}}
	return s
}

// ─── Kimi ─────────────────────────────────────────────────────────────────

func probeKimi(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Kimi"}
	raw, err := os.ReadFile(homePath(".kimi-code", "credentials", "kimi-code.json"))
	if err != nil {
		s.Err = errors.New("~/.kimi-code/credentials/kimi-code.json 不可读")
		return s
	}
	var creds struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil || creds.AccessToken == "" {
		s.Err = errors.New("无法解析 Kimi 凭据")
		return s
	}
	var resp struct {
		User struct {
			Membership struct {
				Level string `json:"level"`
			} `json:"membership"`
		} `json:"user"`
		Usage struct {
			Limit     string `json:"limit"`
			Used      string `json:"used"`
			ResetTime any    `json:"resetTime"`
		} `json:"usage"`
		Limits []struct {
			Detail struct {
				Limit     string `json:"limit"`
				Used      string `json:"used"`
				ResetTime any    `json:"resetTime"`
			} `json:"detail"`
		} `json:"limits"`
	}
	err = getJSON(ctx, "https://api.kimi.com/coding/v1/usages", map[string]string{
		"Authorization": "Bearer " + creds.AccessToken,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	s.Plan = strings.TrimPrefix(resp.User.Membership.Level, "LEVEL_")
	num := func(s string) float64 {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	pct := func(limit, used string) float64 {
		l, u := num(limit), num(used)
		if l <= 0 {
			return -1
		}
		return u / l * 100
	}
	for _, item := range resp.Limits {
		s.Windows = append(s.Windows, Window{
			Label:       "5小时",
			UsedPercent: pct(item.Detail.Limit, item.Detail.Used),
			ResetAt:     parseResetTime(item.Detail.ResetTime),
			Used:        num(item.Detail.Used),
			Total:       num(item.Detail.Limit),
		})
	}
	s.Windows = append(s.Windows, Window{
		Label:       "本周",
		UsedPercent: pct(resp.Usage.Limit, resp.Usage.Used),
		ResetAt:     parseResetTime(resp.Usage.ResetTime),
		Used:        num(resp.Usage.Used),
		Total:       num(resp.Usage.Limit),
	})
	return s
}

// ─── GLM（智谱）────────────────────────────────────────────────────────────

func probeGLM(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "GLM"}
	key := os.Getenv("ZHIPUAI_API_KEY")
	if key == "" {
		key = os.Getenv("GLM_API_KEY")
	}
	if key == "" {
		s.Err = errors.New("未设置 ZHIPUAI_API_KEY")
		return s
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Level  string `json:"level"`
			Limits []struct {
				Type          string  `json:"type"`
				Unit          int     `json:"unit"`
				Usage         float64 `json:"usage"`        // 额度总量（字段名 misleading，实测如此）
				CurrentValue  float64 `json:"currentValue"` // 已用量
				Percentage    float64 `json:"percentage"`
				NextResetTime int64   `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	err := getJSON(ctx, "https://open.bigmodel.cn/api/monitor/usage/quota/limit", map[string]string{
		"Authorization": "Bearer " + key,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	if !resp.Success {
		s.Err = errors.New("接口返回 success=false")
		return s
	}
	s.Plan = resp.Data.Level
	for _, item := range resp.Data.Limits {
		var label string
		switch item.Unit {
		case 3:
			label = "5小时"
		case 6:
			label = "本周"
		default:
			label = "本月"
		}
		if item.Type == "TIME_LIMIT" {
			continue // MCP 次数额度，不在面板展示
		}
		s.Windows = append(s.Windows, Window{
			Label:       label,
			UsedPercent: item.Percentage,
			ResetAt:     epochToTime(item.NextResetTime),
			Used:        item.CurrentValue,
			Total:       item.Usage,
		})
	}
	if len(s.Windows) == 0 {
		s.Err = errors.New("响应中没有额度窗口")
	}
	return s
}

// ─── MiniMax ──────────────────────────────────────────────────────────────

func probeMiniMax(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "MiniMax"}
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		s.Err = errors.New("未设置 MINIMAX_API_KEY")
		return s
	}
	var resp struct {
		ModelRemains []struct {
			ModelName                       string  `json:"model_name"`
			CurrentIntervalRemainingPercent float64 `json:"current_interval_remaining_percent"`
			CurrentWeeklyRemainingPercent   float64 `json:"current_weekly_remaining_percent"`
			CurrentIntervalTotalCount       float64 `json:"current_interval_total_count"`
			CurrentIntervalUsageCount       float64 `json:"current_interval_usage_count"`
			CurrentWeeklyTotalCount         float64 `json:"current_weekly_total_count"`
			CurrentWeeklyUsageCount         float64 `json:"current_weekly_usage_count"`
			EndTime                         int64   `json:"end_time"`
			WeeklyEndTime                   int64   `json:"weekly_end_time"`
		} `json:"model_remains"`
	}
	err := getJSON(ctx, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains", map[string]string{
		"Authorization": "Bearer " + key,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	if len(resp.ModelRemains) == 0 {
		s.Err = errors.New("响应中没有 model_remains")
		return s
	}
	// Coding Plan 的额度在 general 模型上；找不到就用第一条。
	m := resp.ModelRemains[0]
	for _, item := range resp.ModelRemains {
		if item.ModelName == "general" {
			m = item
			break
		}
	}
	s.Plan = "coding plan"
	s.Windows = []Window{
		{
			Label:       "5小时",
			UsedPercent: 100 - m.CurrentIntervalRemainingPercent,
			ResetAt:     epochToTime(m.EndTime),
			Used:        m.CurrentIntervalUsageCount,
			Total:       m.CurrentIntervalTotalCount,
		},
		{
			Label:       "本周",
			UsedPercent: 100 - m.CurrentWeeklyRemainingPercent,
			ResetAt:     epochToTime(m.WeeklyEndTime),
			Used:        m.CurrentWeeklyUsageCount,
			Total:       m.CurrentWeeklyTotalCount,
		},
	}
	return s
}

// ─── Antigravity（agy CLI / Gemini）────────────────────────────────────────

// Antigravity 用的是「installed app」OAuth client，其 client id/secret 嵌在
// 每个用户本机的 agy 二进制里（与 opencode 等第三方集成分享同一对）。
// 本程序不落盘这对值——代码库里放字符串会被 GitHub push protection 拦截——
// 而是需要刷新 token 时从本机 agy 二进制里现找。
const antigravityAPI = "https://daily-cloudcode-pa.googleapis.com/v1internal"

var (
	googleClientIDPattern     = regexp.MustCompile(`[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com`)
	// Google client secret 是「GOCSPX-」+ 固定 28 个字符；不定长匹配会把二进制里
	// 相邻的两个 secret 吞成一个。
	googleClientSecretPattern = regexp.MustCompile(`GOCSPX-[a-zA-Z0-9_-]{28}`)
)

// antigravityClientCreds 从本机 agy 二进制提取 OAuth client id 与 secret 列表。
func antigravityClientCreds() ([]string, []string) {
	path, err := exec.LookPath("agy")
	if err != nil {
		path = homePath(".local", "bin", "agy")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	ids := googleClientIDPattern.FindAllString(string(data), -1)
	secrets := googleClientSecretPattern.FindAllString(string(data), -1)
	return ids, secrets
}

// antigravityToken 从 macOS Keychain 取 agy CLI 保存的 OAuth token
//（service「gemini」、account「antigravity」，go-keyring 的 base64 包装）。
// token 过期时用 refresh_token 换新；不回写 Keychain，本程序只读。
func antigravityToken() (string, string, error) {
	if runtime.GOOS != "darwin" {
		return "", "", errors.New("Antigravity 凭据读取仅支持 macOS Keychain")
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
	if err != nil {
		return "", "", errors.New("Keychain 中没有 agy 凭据（service gemini / account antigravity）")
	}
	raw := strings.TrimSpace(string(out))
	raw = strings.TrimPrefix(raw, "go-keyring-base64:")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(raw); err != nil {
			return "", "", errors.New("无法解码 agy 凭据")
		}
	}
	var stored struct {
		Token struct {
			AccessToken  string    `json:"access_token"`
			RefreshToken string    `json:"refresh_token"`
			Expiry       time.Time `json:"expiry"`
		} `json:"token"`
		AuthMethod string `json:"auth_method"`
	}
	if err := json.Unmarshal(decoded, &stored); err != nil || stored.Token.AccessToken == "" {
		return "", "", errors.New("无法解析 agy 凭据")
	}
	if time.Until(stored.Token.Expiry) > time.Minute || stored.Token.RefreshToken == "" {
		return stored.Token.AccessToken, stored.AuthMethod, nil
	}
	// 过期且有 refresh_token：用二进制里的 client id/secret 组合逐个尝试刷新
	//（二进制里有多对，只有一对与 refresh_token 匹配）。新 token 只在内存里用。
	ids, secrets := antigravityClientCreds()
	for _, id := range ids {
		for _, secret := range secrets {
			form := "client_id=" + id + "&client_secret=" + secret +
				"&grant_type=refresh_token&refresh_token=" + stored.Token.RefreshToken
			req, err := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token",
				strings.NewReader(form))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
			if err != nil {
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				continue
			}
			var refreshed struct {
				AccessToken string `json:"access_token"`
			}
			if err := json.Unmarshal(body, &refreshed); err == nil && refreshed.AccessToken != "" {
				return refreshed.AccessToken, stored.AuthMethod, nil
			}
		}
	}
	return "", "", errors.New("agy token 已过期且刷新失败；请先运行一次 agy 让 CLI 刷新凭据")
}

func probeAntigravity(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Antigravity"}
	token, authMethod, err := antigravityToken()
	if err != nil {
		s.Err = err
		return s
	}
	// 额度挂在每个模型上（quotaInfo.remainingFraction + resetTime）。
	// 注意：不带 User-Agent / X-Goog-Api-Client 时这个端点一律回 429。
	var resp struct {
		Models map[string]struct {
			DisplayName string `json:"displayName"`
			QuotaInfo   *struct {
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	err = postJSON(ctx, antigravityAPI+":fetchAvailableModels", map[string]string{
		"Authorization":    "Bearer " + token,
		"Content-Type":     "application/json",
		"User-Agent":       "antigravity/1.1.11 darwin/arm64",
		"X-Goog-Api-Client": "gl-go/1.24 antigravity/1.1.11",
	}, "{}", &resp)
	if err != nil {
		s.Err = err
		return s
	}

	// 同一模型的高中低推理档共享一族额度：按展示名去掉「(High)」等后缀聚合，
	// 一族取最紧张（remainingFraction 最小）的那个模型来代表。
	type family struct {
		used    float64
		resetAt time.Time
	}
	families := map[string]*family{}
	for _, m := range resp.Models {
		if m.QuotaInfo == nil || m.DisplayName == "" {
			continue
		}
		name := m.DisplayName
		if i := strings.Index(name, " ("); i > 0 {
			name = name[:i]
		}
		used := (1 - m.QuotaInfo.RemainingFraction) * 100
		f, ok := families[name]
		if !ok {
			families[name] = &family{used: used, resetAt: parseResetTime(m.QuotaInfo.ResetTime)}
			continue
		}
		if used > f.used {
			f.used = used
			f.resetAt = parseResetTime(m.QuotaInfo.ResetTime)
		}
	}
	if len(families) == 0 {
		s.Err = errors.New("响应中没有 quotaInfo")
		return s
	}
	// 只展示最新模型那一族，和其他厂商的单一额度口径保持一致。
	// Antigravity 还代理 Claude 等别家模型，这里只取 Gemini 自家模型。
	verRe := regexp.MustCompile(`\d+(?:\.\d+)*`)
	best, bestVer := "", -1.0
	for name, f := range families {
		if !strings.Contains(name, "Gemini") {
			continue
		}
		v, _ := strconv.ParseFloat(verRe.FindString(name), 64)
		if v > bestVer || (v == bestVer && best != "" && f.used > families[best].used) {
			best, bestVer = name, v
		}
	}
	if best == "" { // 没有 Gemini 族就退而取任意一族
		for name := range families {
			best = name
			break
		}
	}
	f := families[best]
	// 与其他厂商的口径统一：行标签是周期而非模型名。该额度的重置点实测
	// 固定在约 5 小时周期的边界上；模型名放进 plan 槽位展示。
	s.Plan = authMethod + " · " + best
	s.Windows = []Window{{Label: "5小时", UsedPercent: f.used, ResetAt: f.resetAt}}
	return s
}

// ─── DeepSeek ─────────────────────────────────────────────────────────────

// deepSeekBalanceOKThreshold 是「余额是否充足」的判定线（元）。
// 面板不展示具体余额，只按这条线显示 100% / 0%。
const deepSeekBalanceOKThreshold = 10.0

func probeDeepSeek(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "DeepSeek"}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		s.Err = errors.New("未设置 DEEPSEEK_API_KEY")
		return s
	}
	var resp struct {
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	err := getJSON(ctx, "https://api.deepseek.com/user/balance", map[string]string{
		"Authorization": "Bearer " + key,
	}, &resp)
	if err != nil {
		s.Err = err
		return s
	}
	if len(resp.BalanceInfos) == 0 {
		s.Err = errors.New("响应中没有 balance_infos")
		return s
	}
	balance, err := strconv.ParseFloat(resp.BalanceInfos[0].TotalBalance, 64)
	if err != nil {
		s.Err = errors.New("无法解析余额")
		return s
	}
	s.Plan = "按量付费"
	// 按量账户没有窗口和重置概念，也不展示具体金额：
	// 余额 >= 阈值显示「可用 100%」（充足），否则「可用 0%」（该充值了）。
	// 这里的百分比是「可用」语义，用 Note 覆盖默认的「已用」前缀。
	pct := 0.0
	if balance >= deepSeekBalanceOKThreshold {
		pct = 100
	}
	s.Windows = []Window{{
		Label:       "余额",
		UsedPercent: pct,
		Note:        fmt.Sprintf("可用 %.0f%%", pct),
	}}
	return s
}

// periodRank 给窗口排层级：短周期在前。无法识别的标签按 0 处理
//（同层不互相覆盖，例如 Antigravity 的模型族名）。
func periodRank(label string) int {
	switch label {
	case "本周":
		return 1
	case "本月":
		return 2
	}
	return 0
}

// applyQuotaHierarchy 处理嵌套额度：5 小时 ⊂ 本周 ⊂ 本月。
// 短周期的实际可用额度受所有更长周期约束——任一长周期用尽，
// 短周期即便自己的窗口计数为零也同样不可用。因此短周期的展示值取
// max(自身已用%, 所有更长周期已用%)，被覆盖时记 ConstrainedBy 供界面标注。
func applyQuotaHierarchy(s *ServiceUsage) {
	for i := range s.Windows {
		wi := &s.Windows[i]
		if wi.UsedPercent < 0 {
			continue
		}
		ri := periodRank(wi.Label)
		for j := range s.Windows {
			wj := &s.Windows[j]
			if j == i || wj.UsedPercent < 0 || periodRank(wj.Label) <= ri {
				continue
			}
			if wj.UsedPercent > wi.UsedPercent {
				wi.UsedPercent = wj.UsedPercent
				wi.ConstrainedBy = wj.Label
			}
		}
	}
}

// ─── 汇总 ─────────────────────────────────────────────────────────────────

func probeAll(ctx context.Context) []ServiceUsage {
	probes := []func(context.Context) ServiceUsage{
		probeClaude, probeCodex, probeGrok, probeKimi, probeGLM, probeMiniMax,
		probeAntigravity, probeDeepSeek,
	}
	results := make([]ServiceUsage, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p func(context.Context) ServiceUsage) {
			defer wg.Done()
			results[i] = p(ctx)
		}(i, p)
	}
	wg.Wait()
	for i := range results {
		applyQuotaHierarchy(&results[i])
	}
	return results
}
