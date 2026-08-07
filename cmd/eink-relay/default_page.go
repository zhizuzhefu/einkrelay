package main

// defaultHelpPageStyle enlarges the base style for the first-boot help page.
// DefaultMarkdownStyle targets dense user documents (18pt body); the help page
// is short instructional text read at arm's length on a 1072x1448 panel, so it
// runs roughly 1.8x with wider margins and gaps.
func defaultHelpPageStyle() MarkdownStyle {
	return MarkdownStyle{
		BaseSize:     28,
		HeadingSizes: [6]float64{48, 38, 34, 30, 28, 28},
		LineSpacing:  1.35,
		ParagraphGap: 16,
		Margin:       44,
		IndentStep:   42,
		QuoteBar:     4,
	}
}

// defaultHelpPageMarkdown is the built-in first-boot screen. A device that has
// never displayed anything has no current/previous to restore; leaving the
// panel white looks like a dead service, so this page is committed through the
// normal display transaction instead. It must stay short enough to fit the
// smallest supported panel without truncation, name no secrets (the token
// itself never appears here), and describe exactly the four things a fresh
// user needs: how to authenticate, how to push content, how to leave, and how
// to check state. Code fences use ~~~ so the literal stays a raw string.
const defaultHelpPageMarkdown = `# EInkRelay 已就绪

设备正处于独占显示模式，通过 USB 或 Wi-Fi 推送内容即可更新画面。

## 1. 获取 Token

~~~sh
TOKEN=$(ssh root@<设备IP> 'cat /var/local/einkrelay/token')
~~~

## 2. 推送内容

显示图片（PNG / JPEG）：

~~~sh
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: image/png" \
  --data-binary @page.png \
  "http://<设备IP>:8080/v1/display/image?fit=contain"
~~~

显示 Markdown：把 @page.png 换成 @page.md，
Content-Type 换成 text/markdown，
路径换成 /v1/display/markdown。

## 3. 退出本界面

**任意一个角落，1 秒内连点 3 次**，即恢复 Kindle 原生界面。

## 4. 状态查询

GET /v1/status（需 token）；完整 API 见 docs/openapi.yaml。
`
