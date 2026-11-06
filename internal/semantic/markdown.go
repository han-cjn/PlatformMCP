package semantic

import (
	"encoding/json"
	"regexp"
	"strings"
)

// markdown → 飞书 post(富文本)结构转换。
//
// 背景:飞书 im_v1_message_create 的 msg_type=post 需要嵌套结构:
//
//	{"zh_cn":{"title":"...","content":[[{"tag":"text","text":"段落1"}],[{"tag":"text","text":"段落2"}]]}}
//
// 而钉钉 msgParam.text 与企微 markdown.content 原生接受 markdown 字符串。
// 因此只有飞书需要转换,放在这里而不是引擎里——保持引擎与平台无关。
//
// 支持范围(刻意保守,不追求完整 CommonMark):
//   - 段落:每个空行分隔的块 → 一行
//   - 粗体 **x** / 斜体 *x* / 行内代码 `x` / 链接 [t](u) / 删除线 ~~x~~
//   - 标题 #~######  → 粗体段落
//   - 无序列表 - / * / +  → 带 • 前缀的独立行
//   - 有序列表 1. 2.      → 保留序号
//
// 不支持的语法降级为纯文本(宁可少渲染,也不要报错或显示乱码)。

// postPost 飞书 post 的语言容器(只填 zh_cn)。
type feishuPost struct {
	ZhCn feishuPostBody `json:"zh_cn"`
}

type feishuPostBody struct {
	Title   string           `json:"title,omitempty"`
	Content [][]feishuInline `json:"content"`
}

// feishuInline 飞书 post 的行内元素,按 tag 区分类型。
type feishuInline struct {
	Tag   string   `json:"tag"`
	Text  string   `json:"text,omitempty"`
	Href  string   `json:"href,omitempty"`
	Style []string `json:"style,omitempty"`
}

var (
	reMdLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMdBold   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reMdItalic = regexp.MustCompile(`\*([^*]+)\*`)
	reMdCode   = regexp.MustCompile("`([^`]+)`")
	reMdStrike = regexp.MustCompile(`~~([^~]+)~~`)
	reHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reUlItem   = regexp.MustCompile(`^[-*+]\s+(.*)$`)
	reOlItem   = regexp.MustCompile(`^(\d+)[.)]\s+(.*)$`)
)

// MarkdownToFeishuPost 把 markdown 文本转为飞书 post 的 {"zh_cn":{...}} JSON 字符串。
// title 为空时使用 markdown 首个标题;都没有则留空(飞书允许无标题)。
func MarkdownToFeishuPost(markdown, title string) (string, error) {
	body := feishuPostBody{Title: title}
	// 统一换行,避免 Windows 风格残留 \r
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")

	for _, block := range splitBlocks(markdown) {
		line, heading, isBold := classifyBlock(block)
		if heading != "" && body.Title == "" {
			body.Title = heading
		}
		inlines := parseInlines(line)
		if isBold {
			for i := range inlines {
				if inlines[i].Tag == "text" {
					inlines[i].Style = append(inlines[i].Style, "bold")
				}
			}
		}
		if len(inlines) > 0 {
			body.Content = append(body.Content, inlines)
		}
	}
	// 空内容给一个空段落,避免飞书报参数错误
	if len(body.Content) == 0 {
		body.Content = [][]feishuInline{{{Tag: "text", Text: ""}}}
	}

	raw, err := json.Marshal(feishuPost{ZhCn: body})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// splitBlocks 把 markdown 切成「渲染行」。
//
// 规则:
//   - 空行 → 块分隔(被丢弃,飞书不需要空段落)
//   - 列表项(- / * / + / 1.) → 每项独立成行,不与相邻内容合并
//   - 其余连续行 → 合并为一个段落(行间用 \n,飞书会在同一 post 行内换行)
//
// 为什么列表必须独立成行:飞书 post 的 content 是 [][]inline,外层每个元素
// 是一个显示行。把多个列表项塞进同一行会渲染成一整行,丢失列表视觉结构。
func splitBlocks(markdown string) []string {
	var blocks []string
	var para []string
	flushPara := func() {
		if len(para) > 0 {
			blocks = append(blocks, strings.Join(para, "\n"))
			para = nil
		}
	}
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flushPara()
			continue
		}
		if isListItem(trimmed) || isHeading(trimmed) {
			flushPara() // 列表/标题必须打断段落
			blocks = append(blocks, trimmed)
			continue
		}
		para = append(para, strings.TrimRight(line, " \t"))
	}
	flushPara()
	return blocks
}

// isListItem 判断是否为无序或有序列表项。
func isListItem(s string) bool {
	return reUlItem.MatchString(s) || reOlItem.MatchString(s)
}

// isHeading 判断是否为 markdown 标题行。
func isHeading(s string) bool {
	return reHeading.MatchString(s)
}

// classifyBlock 识别标题/列表,返回待解析的正文、标题文字、是否需要整行加粗。
func classifyBlock(block string) (line, heading string, bold bool) {
	first := block
	if i := strings.Index(block, "\n"); i >= 0 {
		first = block[:i]
	}
	// 标题:# 后面的文字作为 title,同时作为正文(加粗显示)
	if m := reHeading.FindStringSubmatch(strings.TrimSpace(first)); m != nil {
		text := strings.TrimSpace(m[2])
		return text, text, true
	}
	// 无序列表:整块逐行加 • 前缀
	var out []string
	for _, l := range strings.Split(block, "\n") {
		t := strings.TrimSpace(l)
		if m := reUlItem.FindStringSubmatch(t); m != nil {
			out = append(out, "• "+strings.TrimSpace(m[1]))
			continue
		}
		if m := reOlItem.FindStringSubmatch(t); m != nil {
			out = append(out, m[1]+". "+strings.TrimSpace(m[2]))
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), "", false
}

// parseInlines 把一行 markdown 拆成飞书行内元素序列。
// 顺序:链接 → 粗体 → 斜体 → 行内代码 → 删除线,逐字符扫描避免正则互相干扰。
func parseInlines(line string) []feishuInline {
	if line == "" {
		return nil
	}
	// 先处理链接(整体替换为占位,保留 href)
	var out []feishuInline
	rest := line
	for rest != "" {
		loc := reMdLink.FindStringSubmatchIndex(rest)
		if loc == nil {
			out = append(out, parseEmphasis(rest)...)
			break
		}
		if loc[0] > 0 {
			out = append(out, parseEmphasis(rest[:loc[0]])...)
		}
		text := rest[loc[2]:loc[3]]
		href := rest[loc[4]:loc[5]]
		out = append(out, feishuInline{Tag: "a", Text: text, Href: href})
		rest = rest[loc[1]:]
	}
	return out
}

// parseEmphasis 处理粗体/斜体/代码/删除线。
func parseEmphasis(s string) []feishuInline {
	if s == "" {
		return nil
	}
	// 按优先级顺序匹配,只取最靠前的一个,递归处理其余部分
	type hit struct {
		start, end int
		inline     feishuInline
	}
	var best *hit
	consider := func(re *regexp.Regexp, mk func(string) feishuInline) {
		loc := re.FindStringSubmatchIndex(s)
		if loc == nil {
			return
		}
		if best == nil || loc[0] < best.start {
			best = &hit{start: loc[0], end: loc[1], inline: mk(s[loc[2]:loc[3]])}
		}
	}
	consider(reMdBold, func(t string) feishuInline {
		return feishuInline{Tag: "text", Text: t, Style: []string{"bold"}}
	})
	consider(reMdCode, func(t string) feishuInline {
		return feishuInline{Tag: "text", Text: t, Style: []string{"code"}}
	})
	consider(reMdStrike, func(t string) feishuInline {
		return feishuInline{Tag: "text", Text: t, Style: []string{"lineThrough"}}
	})
	consider(reMdItalic, func(t string) feishuInline {
		return feishuInline{Tag: "text", Text: t, Style: []string{"italic"}}
	})
	if best == nil {
		return []feishuInline{{Tag: "text", Text: s}}
	}
	out := make([]feishuInline, 0, 3)
	if best.start > 0 {
		out = append(out, feishuInline{Tag: "text", Text: s[:best.start]})
	}
	out = append(out, best.inline)
	if best.end < len(s) {
		out = append(out, parseEmphasis(s[best.end:])...)
	}
	return out
}
