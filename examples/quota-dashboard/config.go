package main

import (
	"context"
	"fmt"
	"strings"
)

// provider 是一个可启用/停用的额度来源。ID 用于配置（小写、稳定，
// 改名会破坏用户已有的配置），Name 是面板上的显示名。
type provider struct {
	ID    string
	Name  string
	Probe func(context.Context) ServiceUsage
}

// allProviders 是内置来源的完整清单，切片顺序即默认展示顺序。
// 新增一家服务只需在这里加一行——查询、渲染、配置都会自动带上。
var allProviders = []provider{
	{ID: "claude", Name: "Claude", Probe: probeClaude},
	{ID: "codex", Name: "Codex", Probe: probeCodex},
	{ID: "grok", Name: "Grok", Probe: probeGrok},
	{ID: "kimi", Name: "Kimi", Probe: probeKimi},
	{ID: "glm", Name: "GLM", Probe: probeGLM},
	{ID: "minimax", Name: "MiniMax", Probe: probeMiniMax},
	{ID: "deepseek", Name: "DeepSeek", Probe: probeDeepSeek},
}

func lookupProvider(id string) (provider, bool) {
	for _, p := range allProviders {
		if p.ID == id {
			return p, true
		}
	}
	return provider{}, false
}

// providerIDs 返回全部内置来源 ID，用于用法提示与错误信息。
func providerIDs() []string {
	ids := make([]string, len(allProviders))
	for i, p := range allProviders {
		ids[i] = p.ID
	}
	return ids
}

// resolveProviders 解析「启用哪些额度来源」的配置（-providers 标志或
// DASHBOARD_PROVIDERS 环境变量）。语法是一串逗号分隔的记号，从左到右生效：
//
//	claude,codex        只启用这两家，且按此顺序展示
//	all                 启用全部（默认；空配置等价于 all）
//	all,-grok           启用全部但排除 Grok（!grok 同义）
//
// ID 大小写不敏感，重复出现只算一次。未启用的来源既不查询也不展示——
// 面板上不会为它留「查询失败」的位置。
func resolveProviders(spec string) ([]provider, error) {
	if strings.TrimSpace(spec) == "" {
		spec = "all"
	}

	var enabled []provider
	indexOf := func(id string) int {
		for i, p := range enabled {
			if p.ID == id {
				return i
			}
		}
		return -1
	}

	for _, token := range strings.Split(spec, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		exclude := false
		if strings.HasPrefix(token, "-") || strings.HasPrefix(token, "!") {
			exclude = true
			token = strings.TrimSpace(token[1:])
		}
		id := strings.ToLower(token)

		if id == "all" {
			if exclude {
				enabled = nil
				continue
			}
			for _, p := range allProviders {
				if indexOf(p.ID) < 0 {
					enabled = append(enabled, p)
				}
			}
			continue
		}

		p, ok := lookupProvider(id)
		if !ok {
			return nil, fmt.Errorf("未知的额度来源 %q；可选：%s（或 all）", token, strings.Join(providerIDs(), ", "))
		}
		if exclude {
			if i := indexOf(id); i >= 0 {
				enabled = append(enabled[:i], enabled[i+1:]...)
			}
			continue
		}
		if indexOf(id) < 0 {
			enabled = append(enabled, p)
		}
	}

	if len(enabled) == 0 {
		return nil, fmt.Errorf("配置 %q 没有启用任何额度来源；可选：%s（或 all）", spec, strings.Join(providerIDs(), ", "))
	}
	return enabled, nil
}
