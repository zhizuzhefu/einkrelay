// Command quota-dashboard 汇总本机九家 AI 编程服务的额度使用情况，
// 渲染成一张适合电子墨水屏的灰度 PNG，并推送给 EInkRelay。
//
// 数据源（全部在本机实测可用）：
//
//	Claude      Keychain「Claude Code-credentials」→ api.anthropic.com/api/oauth/usage
//	Codex       ~/.codex/auth.json                → chatgpt.com/backend-api/wham/usage
//	Grok        ~/.grok/auth.json                 → cli-chat-proxy.grok.com/v1/billing?format=credits（周/月额度池）+ /v1/billing（月度 API credits）；套餐名回退 grok.com/rest/subscriptions
//	Kimi        环境变量 KIMI_API_KEY              → api.kimi.com/coding/v1/usages
//	GLM         环境变量 ZHIPUAI_API_KEY           → open.bigmodel.cn/api/monitor/usage/quota/limit
//	MiniMax     环境变量 MINIMAX_API_KEY           → api.minimaxi.com/v1/api/openplatform/coding_plan/remains
//	Antigravity Keychain service「gemini」account「antigravity」→ daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels
//	Auggie      本机 CLI `auggie account status --json` → 本周期剩余/包含额度
//	DeepSeek    环境变量 DEEPSEEK_API_KEY          → api.deepseek.com/user/balance
//
// 用法：
//
//	DASHBOARD_FONT=/path/to/NotoSansCJKsc-Regular.otf \
//	EINKRELAY_HOST=192.168.15.244:8080 EINKRELAY_TOKEN=... \
//	go run .                # 查询九家额度 → 渲染 → 推送到 Kindle
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
	"math"
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
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
		"accept":         "application/json",
		"user-agent":     "quota-dashboard",
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

// probeGrok 读取 Grok CLI 同源的 billing 接口。
//
// 注意：无 query 的 /v1/billing 只返回月度 API credits（monthlyLimit/used），
// 对 SuperGrok / X Premium+ 等统一计费用户并不是真正限制 Build 的额度。
// Grok CLI 的 /usage 用的是 ?format=credits，返回 currentPeriod
//（USAGE_PERIOD_TYPE_WEEKLY / MONTHLY）和可选的 creditUsagePercent。
// creditUsagePercent 是 proto3 标量：已用 0% 时字段会被省略，不是「未知」。
// subscriptionTier 现在经常也不在 billing 里，套餐名回退到 grok.com/rest/subscriptions。
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
	auth := map[string]string{"Authorization": "Bearer " + key}

	// 1) format=credits：CLI 同源的周/月额度池（统一计费用户以周为主）
	var credits struct {
		Config struct {
			CreditUsagePercent json.RawMessage `json:"creditUsagePercent"`
			CurrentPeriod      *struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
			MonthlyLimit struct {
				Val float64 `json:"val"`
			} `json:"monthlyLimit"`
			Used struct {
				Val float64 `json:"val"`
			} `json:"used"`
			BillingPeriodEnd     string `json:"billingPeriodEnd"`
			IsUnifiedBillingUser bool   `json:"isUnifiedBillingUser"`
		} `json:"config"`
		SubscriptionTier      string `json:"subscriptionTier"`
		SubscriptionTierSnake string `json:"subscription_tier"`
	}
	if err := getJSON(ctx, "https://cli-chat-proxy.grok.com/v1/billing?format=credits", auth, &credits); err != nil {
		s.Err = err
		return s
	}
	s.Plan = prettyGrokTier(credits.SubscriptionTier)
	if s.Plan == "" {
		s.Plan = prettyGrokTier(credits.SubscriptionTierSnake)
	}

	if len(credits.Config.CreditUsagePercent) > 0 || credits.Config.CurrentPeriod != nil {
		label := "本周"
		resetAt := parseResetTime(credits.Config.BillingPeriodEnd)
		if p := credits.Config.CurrentPeriod; p != nil {
			switch {
			case strings.Contains(p.Type, "MONTHLY"):
				label = "本月"
			case strings.Contains(p.Type, "WEEKLY"):
				label = "本周"
			}
			if t := parseResetTime(p.End); !t.IsZero() {
				resetAt = t
			}
		} else if !credits.Config.IsUnifiedBillingUser {
			label = "本月"
		}
		s.Windows = append(s.Windows, Window{
			Label:       label,
			UsedPercent: grokUsagePercent(credits.Config.CreditUsagePercent),
			ResetAt:     resetAt,
		})
	}

	// 2) 月度 API credits：format=credits 若已带 monthlyLimit 则复用，否则再查裸端点
	monthLimit := credits.Config.MonthlyLimit.Val
	monthUsed := credits.Config.Used.Val
	monthEnd := credits.Config.BillingPeriodEnd
	if monthLimit <= 0 {
		var monthly struct {
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
		if err := getJSON(ctx, "https://cli-chat-proxy.grok.com/v1/billing", auth, &monthly); err == nil {
			monthLimit = monthly.Config.MonthlyLimit.Val
			monthUsed = monthly.Config.Used.Val
			monthEnd = monthly.Config.BillingPeriodEnd
		}
	}
	if monthLimit > 0 {
		pct := monthUsed / monthLimit * 100
		enriched := false
		for i := range s.Windows {
			if s.Windows[i].Label != "本月" {
				continue
			}
			// 同一「本月」行补上绝对量；百分比以额度池为准（若已有）
			s.Windows[i].Used = monthUsed
			s.Windows[i].Total = monthLimit
			if s.Windows[i].UsedPercent < 0 {
				s.Windows[i].UsedPercent = pct
			}
			if s.Windows[i].ResetAt.IsZero() {
				s.Windows[i].ResetAt = parseResetTime(monthEnd)
			}
			enriched = true
			break
		}
		if !enriched {
			s.Windows = append(s.Windows, Window{
				Label:       "本月",
				UsedPercent: pct,
				ResetAt:     parseResetTime(monthEnd),
				Used:        monthUsed,
				Total:       monthLimit,
			})
		}
		if s.Plan == "" {
			s.Plan = fmt.Sprintf("%.0f/%.0f credits", monthUsed, monthLimit)
		}
	}

	if s.Plan == "" {
		s.Plan = grokSubscriptionPlan(ctx, auth)
	}

	if len(s.Windows) == 0 {
		s.Err = errors.New("billing 响应中没有可用额度")
	}
	return s
}

// grokUsagePercent 把 billing 里的 creditUsagePercent 收成 0–100。
// proto3 标量 0 会被省略；也兼容 {val: N} 包装。
func grokUsagePercent(raw json.RawMessage) float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var wrap struct {
		Val float64 `json:"val"`
	}
	if json.Unmarshal(raw, &wrap) == nil {
		return wrap.Val
	}
	return 0
}

func prettyGrokTier(tier string) string {
	t := strings.TrimSpace(tier)
	t = strings.TrimPrefix(t, "SUBSCRIPTION_TIER_")
	if t == "" {
		return ""
	}
	switch strings.ToUpper(strings.ReplaceAll(t, " ", "_")) {
	case "X_PREMIUM_PLUS":
		return "X Premium+"
	case "X_PREMIUM":
		return "X Premium"
	case "SUPERGROK", "SUPER_GROK":
		return "SuperGrok"
	case "SUPERGROK_HEAVY", "SUPER_GROK_HEAVY":
		return "SuperGrok Heavy"
	default:
		return t
	}
}

// grokSubscriptionPlan 在 billing 没给套餐名时，从 grok.com 订阅接口补。
func grokSubscriptionPlan(ctx context.Context, auth map[string]string) string {
	var resp struct {
		Subscriptions []struct {
			Tier   string `json:"tier"`
			Status string `json:"status"`
		} `json:"subscriptions"`
	}
	if err := getJSON(ctx, "https://grok.com/rest/subscriptions", auth, &resp); err != nil {
		return ""
	}
	for _, sub := range resp.Subscriptions {
		if sub.Status != "" && !strings.Contains(sub.Status, "ACTIVE") {
			continue
		}
		if p := prettyGrokTier(sub.Tier); p != "" {
			return p
		}
	}
	return ""
}

// ─── Kimi ─────────────────────────────────────────────────────────────────

func probeKimi(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Kimi"}
	apiKey := os.Getenv("KIMI_API_KEY")
	if apiKey == "" {
		s.Err = errors.New("未设置 KIMI_API_KEY")
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
	err := getJSON(ctx, "https://api.kimi.com/coding/v1/usages", map[string]string{
		"Authorization": "Bearer " + apiKey,
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

const antigravityAPI = "https://daily-cloudcode-pa.googleapis.com/v1internal"

// antigravityToken 从 macOS Keychain 取 agy CLI 保存的 OAuth token
// （service「gemini」、account「antigravity」，go-keyring 的 base64 包装）。
// 本程序只读凭据，不刷新、不回写；token 过期时请运行一次 agy 让它自己续期。
func antigravityToken() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("Antigravity 凭据读取仅支持 macOS Keychain")
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "gemini", "-a", "antigravity", "-w").Output()
	if err != nil {
		return "", errors.New("Keychain 中没有 agy 凭据（service gemini / account antigravity）")
	}
	raw := strings.TrimSpace(string(out))
	raw = strings.TrimPrefix(raw, "go-keyring-base64:")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(raw); err != nil {
			return "", errors.New("无法解码 agy 凭据")
		}
	}
	var stored struct {
		Token struct {
			AccessToken string    `json:"access_token"`
			Expiry      time.Time `json:"expiry"`
		} `json:"token"`
	}
	if err := json.Unmarshal(decoded, &stored); err != nil || stored.Token.AccessToken == "" {
		return "", errors.New("无法解析 agy 凭据")
	}
	if time.Until(stored.Token.Expiry) <= time.Minute {
		return "", errors.New("agy token 已过期；请先运行一次 agy 让它刷新凭据")
	}
	return stored.Token.AccessToken, nil
}

// modelVersionRe 匹配模型名中的第一个版本号（如 "Gemini 3.10 Flash" 的 3.10）。
var modelVersionRe = regexp.MustCompile(`\d+(?:\.\d+)*`)

// versionSegments 把名字中的第一个版本号按段拆成整数；没有版本号时返回 nil。
func versionSegments(name string) []int {
	parts := strings.Split(modelVersionRe.FindString(name), ".")
	segs := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		segs = append(segs, n)
	}
	return segs
}

// compareVersions 按段比较版本号：3.10 > 3.9（ParseFloat 会把 3.10 当成 3.1）。
// 前缀相同则段数多的更大；a 大返回正数，相等返回 0。
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return len(a) - len(b)
}

func probeAntigravity(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Antigravity"}
	token, err := antigravityToken()
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
		"Authorization":     "Bearer " + token,
		"Content-Type":      "application/json",
		"User-Agent":        "antigravity/1.1.11 darwin/arm64",
		"X-Goog-Api-Client": "gl-go/1.24 antigravity/1.1.11",
	}, "{}", &resp)
	if err != nil {
		s.Err = err
		return s
	}

	// 同一模型的高中低推理档共享一族额度：按展示名去掉「(High)」等后缀聚合，
	// 一族取最紧张（remainingFraction 最小）的那个模型来代表。
	// 没有展示名的条目（多为尚未正式发布的新模型）用模型 id 去掉档级后缀兜底，
	// 保证新模型一出现就能参与「最新模型」的竞选。
	type family struct {
		used    float64
		resetAt time.Time
	}
	families := map[string]*family{}
	for id, m := range resp.Models {
		if m.QuotaInfo == nil {
			continue
		}
		name := m.DisplayName
		if i := strings.Index(name, " ("); i > 0 {
			name = name[:i]
		}
		if name == "" {
			name = strings.TrimSuffix(id, "-tiered")
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
	// 版本号按段比较（3.10 > 3.9），不能用 ParseFloat。
	best := ""
	var bestVer []int
	for name, f := range families {
		if !strings.Contains(strings.ToLower(name), "gemini") {
			continue
		}
		v := versionSegments(name)
		if v == nil {
			continue
		}
		if best == "" || compareVersions(v, bestVer) > 0 ||
			(compareVersions(v, bestVer) == 0 && f.used > families[best].used) {
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
	// 固定在约 5 小时周期的边界上；自动选出的最新模型族只用于取额度，
	// 不展示具体模型名（Plan 留空）。
	s.Windows = []Window{{Label: "5小时", UsedPercent: f.used, ResetAt: f.resetAt}}
	return s
}

// auggieStatus 是 auggie account status --json 中面板所需的字段。
// 其余字段（包括 banner）故意不解析，避免把账户信息带入结果。
type auggieStatus struct {
	PlanName               string `json:"planName"`
	UsageUnit              string `json:"usageUnit"`
	AmountRemaining        string `json:"amountRemaining"`
	AmountIncludedPerCycle string `json:"amountIncludedPerCycle"`
	BillingCycleEndDate    string `json:"billingCycleEndDate"`
}

func parseAuggieAmount(raw, field string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("Auggie %s 不是有效数字", field)
	}
	return n, nil
}

// parseAuggieWindow 解析 Auggie 的字符串额度并计算本周期窗口。
// Used 保留原始绝对量（即使数据超出边界），只有百分比做 0–100 保护。
func parseAuggieWindow(status auggieStatus) (Window, error) {
	remaining, err := parseAuggieAmount(status.AmountRemaining, "amountRemaining")
	if err != nil {
		return Window{}, err
	}
	included, err := parseAuggieAmount(status.AmountIncludedPerCycle, "amountIncludedPerCycle")
	if err != nil {
		return Window{}, err
	}
	if included <= 0 {
		return Window{}, errors.New("Auggie amountIncludedPerCycle 必须大于 0")
	}
	used := included - remaining
	if math.IsInf(used, 0) || math.IsNaN(used) {
		return Window{}, errors.New("Auggie 额度计算结果超出范围")
	}
	pct := used / included * 100
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return Window{
		Label:       "本周期",
		UsedPercent: pct,
		ResetAt:     parseResetTime(status.BillingCycleEndDate),
		Used:        used,
		Total:       included,
	}, nil
}

func auggiePlan(planName, usageUnit string) string {
	plan := strings.TrimSpace(planName)
	unit := strings.TrimSpace(usageUnit)
	if plan == "" {
		return unit
	}
	if unit == "" || strings.Contains(strings.ToLower(plan), strings.ToLower(unit)) {
		return plan
	}
	return plan + " · " + unit
}

func probeAuggie(ctx context.Context) ServiceUsage {
	s := ServiceUsage{Name: "Auggie"}
	if err := ctx.Err(); err != nil {
		s.Err = err
		return s
	}
	out, err := exec.CommandContext(ctx, "auggie", "account", "status", "--json").Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			s.Err = ctxErr
		} else {
			s.Err = errors.New("Auggie CLI 不可用或执行失败")
		}
		return s
	}
	if err := ctx.Err(); err != nil {
		s.Err = err
		return s
	}
	var status auggieStatus
	if err := json.Unmarshal(out, &status); err != nil {
		s.Err = errors.New("无法解析 Auggie 状态 JSON")
		return s
	}
	window, err := parseAuggieWindow(status)
	if err != nil {
		s.Err = err
		return s
	}
	s.Plan = auggiePlan(status.PlanName, status.UsageUnit)
	s.Windows = []Window{window}
	return s
}

// ─── DeepSeek ─────────────────────────────────────────────────────────────

// deepSeekBalanceFull 是按量余额换算百分比时的满额基准（元）。
// 余额 ≥ 此值视为可用 100% / 已用 0%；更低时按比例换算，例如
// 剩余 900 → 已用 10% · 可用 90%。面板不展示具体金额。
const deepSeekBalanceFull = 1000.0

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
	if balance < 0 {
		balance = 0
	}
	s.Plan = "按量付费"
	// 按量账户没有窗口和重置：以 deepSeekBalanceFull 为满额基准做线性换算。
	// 进度条与其它服务一致按「已用」填充；文案同时给出已用/可用百分比。
	avail := balance
	if avail > deepSeekBalanceFull {
		avail = deepSeekBalanceFull
	}
	usedPct := (deepSeekBalanceFull - avail) / deepSeekBalanceFull * 100
	availPct := avail / deepSeekBalanceFull * 100
	s.Windows = []Window{{
		Label:       "余额",
		UsedPercent: usedPct,
		Note:        fmt.Sprintf("已用 %.0f%% · 可用 %.0f%%", usedPct, availPct),
	}}
	return s
}

// periodRank 给窗口排层级：短周期在前。无法识别的标签按 0 处理
// （同层不互相覆盖，例如 Antigravity 的模型族名）。
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
// 各周期窗口相互独立，平时各自展示自身的已用百分比；只有当某个更长
// 周期已经用尽（100%）时，短周期才同样不可用——此时把短周期也显示为
// 100%，并记 ConstrainedBy 供界面标注限制来源。
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
			if wj.UsedPercent >= 100 {
				wi.UsedPercent = 100
				wi.ConstrainedBy = wj.Label
			}
		}
	}
}

// ─── 汇总 ─────────────────────────────────────────────────────────────────

func probeAll(ctx context.Context) []ServiceUsage {
	probes := []func(context.Context) ServiceUsage{
		probeClaude, probeCodex, probeGrok, probeKimi, probeGLM, probeMiniMax,
		probeDeepSeek, probeAntigravity, probeAuggie,
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
