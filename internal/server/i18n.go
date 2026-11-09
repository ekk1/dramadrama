package server

import (
	"net/http"
	"regexp"
	"strings"
)

// Interface languages. English strings are the message keys; other
// languages map them to translations. Missing entries fall back to English.
const (
	LangEN = "en"
	LangZH = "zh"
)

const langCookie = "lang"

// langOf picks the UI language: cookie first, then Accept-Language.
func langOf(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && (c.Value == LangEN || c.Value == LangZH) {
		return c.Value
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "zh"):
			return LangZH
		case strings.HasPrefix(tag, "en"):
			return LangEN
		}
	}
	return LangEN
}

// translator returns the lookup function for a language. It translates
// exact keys, then dynamic messages via patterns; unknown text is returned
// unchanged.
func translator(lang string) func(string) string {
	if lang != LangZH {
		return func(s string) string { return s }
	}
	var tr func(string) string
	tr = func(s string) string {
		if s == "" {
			return s
		}
		if v, ok := zh[s]; ok {
			return v
		}
		for _, p := range zhPatterns {
			if m := p.re.FindStringSubmatch(s); m != nil {
				return p.fn(m, tr)
			}
		}
		return s
	}
	return tr
}

type pattern struct {
	re *regexp.Regexp
	fn func(m []string, tr func(string) string) string
}

func pat(expr string, fn func(m []string, tr func(string) string) string) pattern {
	return pattern{regexp.MustCompile(expr), fn}
}

// zhPatterns translates messages that embed values.
var zhPatterns = []pattern{
	pat(`^Calling (.+) / (.+)…$`, func(m []string, _ func(string) string) string {
		return "正在调用 " + m[1] + " / " + m[2] + "…"
	}),
	pat(`^Generating with (.+) / (.+) \((\d+) reference image\(s\)\)…$`, func(m []string, _ func(string) string) string {
		return "正在使用 " + m[1] + " / " + m[2] + " 生成（" + m[3] + " 张参考图）…"
	}),
	pat(`^Saved (\S+)$`, func(m []string, _ func(string) string) string { return "已保存 " + m[1] }),
	pat(`^── Shot (\d+) \((\d+)/(\d+)\)$`, func(m []string, _ func(string) string) string {
		return "── 镜头 " + m[1] + "（" + m[2] + "/" + m[3] + "）"
	}),
	pat(`^── (.+) \((\d+)/(\d+)\)$`, func(m []string, _ func(string) string) string {
		return "── " + m[1] + "（" + m[2] + "/" + m[3] + "）"
	}),
	pat(`^Error: (.*)$`, func(m []string, tr func(string) string) string { return "错误：" + tr(m[1]) }),
	pat(`^Skipping reference (\S+): larger than 5 MB\.$`, func(m []string, _ func(string) string) string {
		return "跳过参考图 " + m[1] + "：超过 5 MB。"
	}),
	pat(`^shot (\d+) has no keyframe prompt yet: run step 5 first$`, func(m []string, _ func(string) string) string {
		return "镜头 " + m[1] + " 还没有关键帧提示词：请先运行第 5 步"
	}),
	pat(`^(.+) has no prompt$`, func(m []string, _ func(string) string) string { return m[1] + " 没有图像提示词" }),
	pat(`^provider "(.+)" has no API key: set the (\S+) environment variable or store a key in Settings$`, func(m []string, _ func(string) string) string {
		return "服务商 “" + m[1] + "” 没有 API 密钥：请设置环境变量 " + m[2] + "，或在设置中保存密钥"
	}),
	pat(`^provider "(.+)" has no API key: add one in Settings$`, func(m []string, _ func(string) string) string {
		return "服务商 “" + m[1] + "” 没有 API 密钥：请在设置中添加"
	}),
	pat(`^unknown provider "(.+)"$`, func(m []string, _ func(string) string) string { return "未知的服务商 “" + m[1] + "”" }),
	pat(`^provider "(.+)" is not an LLM provider$`, func(m []string, _ func(string) string) string {
		return "服务商 “" + m[1] + "” 不是 LLM 服务商"
	}),
	pat(`^"(.+)" is not a configured image provider$`, func(m []string, _ func(string) string) string {
		return "“" + m[1] + "” 不是已配置的图像服务商"
	}),
	pat(`^could not parse the output as JSON \((.*)\); the raw text was saved so you can fix and apply it$`, func(m []string, tr func(string) string) string {
		return "无法将输出解析为 JSON（" + tr(m[1]) + "）；原始文本已保存，可修改后应用"
	}),
	pat(`^could not parse the output as JSON: (.*)$`, func(m []string, _ func(string) string) string {
		return "无法将输出解析为 JSON：" + m[1]
	}),
	pat(`^(\d+) of (\d+) failed: (.*)$`, func(m []string, _ func(string) string) string {
		return m[2] + " 项中有 " + m[1] + " 项失败：" + strings.ReplaceAll(m[3], "Shot ", "镜头 ")
	}),
	pat(`^model returned no text \(finish reason "(.*)"\)$`, func(m []string, _ func(string) string) string {
		return "模型没有返回文本（结束原因 “" + m[1] + "”）"
	}),
}

// jsKeys are the strings app.js needs; they are passed to the page as a map.
var jsKeys = []string{
	"Running…", "Loading…", "No assets found.", "Failed to load: ", "Failed: ",
	"done", "finished",
}

func jsStrings(tr func(string) string) map[string]string {
	m := make(map[string]string, len(jsKeys))
	for _, k := range jsKeys {
		m[k] = tr(k)
	}
	return m
}

// zh is the Simplified Chinese dictionary.
var zh = map[string]string{
	// Layout / navigation
	"Projects":                 "项目",
	"Asset library":            "素材库",
	"Prompts":                  "提示词",
	"Settings":                 "设置",
	"Dismiss":                  "关闭",
	"Pick from asset library":  "从素材库选择",
	"This project":             "本项目",
	"All projects":             "全部项目",
	"Any kind":                 "全部类型",
	"Search name, prompt, tag": "搜索名称、提示词、标签",
	"Close":                    "关闭",
	"Language":                 "语言",
	"Prompt templates":         "提示词模板",

	// Kinds
	"reference": "参考图",
	"concept":   "概念图",
	"keyframe":  "关键帧",
	"video":     "视频",
	"character": "角色",
	"scene":     "场景",
	"prop":      "道具",
	"style":     "风格",

	// Steps
	"idea":           "构思扩写",
	"visual_prompts": "视觉提示词",
	"storyboard":     "分镜概览",
	"shot_prompts":   "镜头提示词",

	// Partials
	"+ From library":            "+ 从素材库",
	"Upload":                    "上传",
	"in use":                    "使用中",
	"use":                       "使用",
	"Use this image downstream": "后续步骤使用这张图",
	"unlink":                    "移除",
	"Unlink (the asset stays in the library)": "移除关联（素材仍保留在素材库中）",
	"None.":                          "无。",
	"Provider":                       "服务商",
	"Model":                          "模型",
	"model id":                       "模型 ID",
	"auto":                           "自动",
	"with refs":                      "有参考图时",
	"only missing":                   "仅缺失项",
	"running…":                       "运行中…",
	"done":                           "完成",
	"error":                          "出错",
	"stop":                           "停止",
	"close":                          "关闭",
	"interrupted (server restarted)": "已中断（服务重启）",
	"stopped":                        "已停止",

	// Project page
	"← Projects":     "← 项目",
	"Steps":          "步骤",
	"1 Input":        "1 输入",
	"2 Story":        "2 故事",
	"3 Visuals":      "3 视觉",
	"4 Storyboard":   "4 分镜",
	"5 Shot prompts": "5 镜头提示词",
	"6 Keyframes":    "6 关键帧",
	"Input":          "输入",
	"Project name":   "项目名称",
	"Aspect ratio":   "画幅比例",
	"Core idea":      "核心创意",
	"Describe the concept in a few sentences…": "用几句话描述你的创意…",
	"Requirements": "要求",
	"(optional: episode length, number of shots, tone, audience, …)": "（可选：单集时长、镜头数量、基调、受众等）",
	"Save input":             "保存输入",
	"Style reference images": "风格参考图",
	"No reference images yet. They are sent to vision models in step 3 (configurable in Settings) and can be attached to any image generation.": "还没有参考图。参考图会在第 3 步发送给视觉模型（可在设置中调整），也可以附加到任意图像生成中。",
	"Idea expansion":                         "构思扩写",
	"Expand idea":                            "扩写构思",
	"Story":                                  "故事",
	"(editable; later steps read this text)": "（可编辑；后续步骤读取这段文字）",
	"Save story":                             "保存故事",
	"Visual development":                     "视觉开发",
	"3a · Visual prompts":                    "3a · 视觉提示词",
	"Generate visual prompts":                "生成视觉提示词",
	"Re-running replaces the list. Items with the same kind and name keep their images.": "重新运行会替换列表。类型和名称相同的条目会保留已生成的图片。",
	"The last output could not be parsed as JSON. Fix it below and apply.":               "上次的输出无法解析为 JSON。请在下方修改后应用。",
	"Apply JSON":                       "应用 JSON",
	"Global style":                     "全局风格",
	"(appended to every image prompt)": "（附加到每条图像提示词末尾）",
	"Save style":                       "保存风格",
	"3b · Concept images":              "3b · 概念图",
	"Generate all images":              "生成全部图片",
	"Kind":                             "类型",
	"Name":                             "名称",
	"Description":                      "描述",
	"Image prompt":                     "图像提示词",
	"Save":                             "保存",
	"Generate image":                   "生成图片",
	"Delete this item? Its images stay in the library.": "删除此条目？其图片仍保留在素材库中。",
	"delete item":                        "删除条目",
	"Generated images":                   "已生成图片",
	"Not generated yet.":                 "尚未生成。",
	"Conditioning references":            "条件参考图",
	"None: text-to-image model is used.": "无：将使用文生图模型。",
	"No visual items yet. Generate them from the story or add them by hand.": "还没有视觉条目。可以从故事生成，也可以手动添加。",
	"+ Add item":          "+ 添加条目",
	"Storyboard overview": "分镜概览",
	"Generate shot list":  "生成镜头列表",
	"Re-running replaces all shots (and their links to prompts and keyframes; the images stay in the library).": "重新运行会替换全部镜头（以及它们关联的提示词和关键帧；图片仍保留在素材库中）。",
	"Scene":                                "场景",
	"Characters":                           "角色",
	"comma separated":                      "用逗号分隔",
	"Camera":                               "镜头运动",
	"Seconds":                              "秒数",
	"Action":                               "动作",
	"Dialogue":                             "台词",
	"Move up":                              "上移",
	"Move down":                            "下移",
	"Insert a shot after this one":         "在此后插入镜头",
	"+ after":                              "+ 插入",
	"delete":                               "删除",
	"Delete this shot?":                    "删除这个镜头？",
	"No shots yet.":                        "还没有镜头。",
	"+ Add shot":                           "+ 添加镜头",
	"Shot-level prompts":                   "镜头级提示词",
	"Generate for all shots":               "为全部镜头生成",
	"Storyboard description":               "分镜描述",
	"Keyframe prompt":                      "关键帧提示词",
	"(English, used for image generation)": "（英文，用于图像生成）",
	"Motion prompt":                        "运动提示词",
	"(for image-to-video later)":           "（供以后图生视频使用）",
	"Generate prompts":                     "生成提示词",
	"Create the storyboard first.":         "请先创建分镜。",
	"Keyframes":                            "关键帧",
	"Generate all keyframes":               "生成全部关键帧",
	"No keyframe prompt yet: run step 5.":  "还没有关键帧提示词：请运行第 5 步。",
	"Generate keyframe":                    "生成关键帧",
	"Auto-selected from concept images (add references to override):": "已从概念图中自动选择（添加参考图可覆盖）：",
	"No matching concept images: text-to-image model is used.":        "没有匹配的概念图：将使用文生图模型。",
	"Delete this project? Its assets stay in the library.":            "删除此项目？其素材仍保留在素材库中。",
	"Delete project": "删除项目",

	// Projects list
	"New project":              "新建项目",
	"Untitled":                 "未命名",
	"What is the drama about?": "这部短剧讲什么？",
	"Create":                   "创建",
	"story":                    "故事",
	"visuals":                  "视觉条目",
	"shots":                    "镜头",
	"keyframes":                "关键帧",
	"Updated":                  "更新于",
	"No projects yet.":         "还没有项目。",

	// Asset library
	"Any tag":           "全部标签",
	"Filter":            "筛选",
	"reset":             "重置",
	"Upload to library": "上传到素材库",
	"Files":             "文件",
	"Project":           "项目",
	"(none: shared)":    "（无：共享）",
	"Tags":              "标签",
	"asset(s)":          "个素材",
	"(deleted project)": "（已删除的项目）",
	"Details / edit":    "详情 / 编辑",
	"model":             "模型",
	"inputs":            "输入素材",
	"download":          "下载",
	"Delete this asset permanently? It will be unlinked from all projects.": "永久删除此素材？它将从所有项目中移除。",
	"delete permanently": "永久删除",
	"No assets match.":   "没有匹配的素材。",

	// Settings
	"Providers": "服务商",
	"API keys can be stored in the config file or read from an environment variable. Stored keys are never shown again; leave the field empty to keep the current key.": "API 密钥可以保存在配置文件中，也可以从环境变量读取。已保存的密钥不会再显示；留空即保留当前密钥。",
	"Type":                         "类型",
	"LLM":                          "LLM",
	"Image":                        "图像",
	"Base URL":                     "Base URL",
	"provider default":             "服务商默认地址",
	"API key":                      "API 密钥",
	"•••• stored":                  "•••• 已保存",
	"not stored":                   "未保存",
	"…or env var":                  "…或环境变量",
	"set":                          "已设置",
	"not set":                      "未设置",
	"Extra headers":                "附加请求头",
	`(one "Name: value" per line)`: "（每行一个 “Name: value”）",
	"clear stored key":             "清除已保存的密钥",
	"delete provider":              "删除服务商",
	"Add provider":                 "添加服务商",
	"Default model per step":       "各步骤默认模型",
	"Each run can override provider and model on the project page. Temperature is optional and ignored by Anthropic.": "每次运行都可以在项目页面覆盖服务商和模型。温度为可选项，Anthropic 会忽略。",
	"Step":                        "步骤",
	"Temperature":                 "温度",
	"Max tokens":                  "最大 token 数",
	"Send reference images":       "发送参考图",
	"(none)":                      "（无）",
	"Image generation":            "图像生成",
	"Default aspect ratio":        "默认画幅比例",
	"Text-to-image model":         "文生图模型",
	"Model with reference images": "带参考图时的模型",
	"Extra parameters":            "附加参数",
	`(JSON object merged into every request, e.g. {"num_inference_steps": 28})`: `（合并到每个请求中的 JSON 对象，例如 {"num_inference_steps": 28}）`,
	"Save settings": "保存设置",

	// Prompts page
	"These Go text/template files build every LLM request. Edits are saved as overrides in": "这些 Go text/template 模板用于构建每个 LLM 请求。修改会作为覆盖文件保存在",
	"and take effect on the next run. Available data:":                                      "，下次运行即生效。可用数据：",
	"(project: Idea, Requirements, Story, Style, Visuals, Shots, AspectRatio)":              "（项目：Idea、Requirements、Story、Style、Visuals、Shots、AspectRatio）",
	"function":         "函数",
	"customised":       "已自定义",
	"reset to default": "恢复默认",

	// Flash messages and handler errors
	"Saved": "已保存",
	"Project deleted. Its assets remain in the library.": "项目已删除，其素材仍保留在素材库中。",
	"that step is not running":                           "该步骤没有在运行",
	"visual item not found":                              "未找到视觉条目",
	"shot not found":                                     "未找到镜头",
	"no files uploaded":                                  "没有上传文件",
	"asset not found":                                    "未找到素材",
	"project not found":                                  "未找到项目",
	"cross-origin request rejected":                      "已拒绝跨站请求",

	// Pipeline messages
	"this step is already running":                                                          "该步骤已在运行",
	"enter a core idea first":                                                               "请先填写核心创意",
	"the story is empty: run idea expansion or write the story first":                       "故事为空：请先运行构思扩写或手动填写故事",
	"no provider/model configured for this step; set one in Settings or in the run options": "该步骤未配置服务商/模型；请在设置或运行选项中指定",
	"no image model configured; set one in Settings":                                        "未配置图像模型；请在设置中指定",
	"nothing to generate":                                                                   "没有需要生成的内容",
	"no shots to process":                                                                   "没有需要处理的镜头",
	"visual item was deleted":                                                               "视觉条目已被删除",
	"shot was deleted":                                                                      "镜头已被删除",
	`no "items" in output`:                                                                  `输出中没有 "items"`,
	`no "shots" in output`:                                                                  `输出中没有 "shots"`,
	"output has no keyframe_prompt":                                                         "输出中没有 keyframe_prompt",
	"Warning: the output hit the token limit and may be truncated.":                         "警告：输出达到 token 上限，可能被截断。",
	"Only the first 6 reference images are sent to the model.":                              "只会向模型发送前 6 张参考图。",

	// JS
	"Running…":         "运行中…",
	"Loading…":         "加载中…",
	"No assets found.": "没有找到素材。",
	"Failed to load: ": "加载失败：",
	"Failed: ":         "失败：",
	"finished":         "已结束",
}
