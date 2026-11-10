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

// zhRoles names the inputs of a video request.
var zhRoles = map[string]string{
	"first frame": "首帧", "last frame": "尾帧", "reference images": "参考图",
	"reference videos": "参考视频", "reference audio": "参考音频",
}

// zhPatterns translates messages that embed values.
var zhPatterns = []pattern{
	pat(`^Calling (.+) / (.+)…$`, func(m []string, _ func(string) string) string {
		return "正在调用 " + m[1] + " / " + m[2] + "…"
	}),
	pat(`^Generating with (.+) / (.+) \((\d+) reference image\(s\)\)…$`, func(m []string, _ func(string) string) string {
		return "正在使用 " + m[1] + " / " + m[2] + " 生成（" + m[3] + " 张参考图）…"
	}),
	pat(`^Fetched (\d+) models from (.+)$`, func(m []string, _ func(string) string) string {
		return "已从 " + m[2] + " 拉取 " + m[1] + " 个模型"
	}),
	pat(`^Collecting the result of request (\S+), submitted earlier, instead of generating again…$`, func(m []string, _ func(string) string) string {
		return "正在取回之前提交的请求 " + m[1] + " 的结果，不会重新生成…"
	}),
	pat(`^Provider status: IN_QUEUE \(position (\d+)\)$`, func(m []string, _ func(string) string) string {
		return "服务商状态：排队中（第 " + m[1] + " 位）"
	}),
	pat(`^Provider status: IN_QUEUE$`, func(_ []string, _ func(string) string) string { return "服务商状态：排队中" }),
	pat(`^Provider status: IN_PROGRESS$`, func(_ []string, _ func(string) string) string { return "服务商状态：生成中" }),
	pat(`^Provider status: (.+)$`, func(m []string, _ func(string) string) string { return "服务商状态：" + m[1] }),
	pat(`^Fetching the parameter list of (.+)…$`, func(m []string, _ func(string) string) string {
		return "正在拉取 " + m[1] + " 的参数列表…"
	}),
	pat(`^Could not fetch the model's parameter list: (.*)$`, func(m []string, _ func(string) string) string {
		return "无法拉取模型的参数列表：" + m[1]
	}),
	pat(`^Generating video with (.+) / (.+)…$`, func(m []string, _ func(string) string) string {
		return "正在使用 " + m[1] + " / " + m[2] + " 生成视频…"
	}),
	pat(`^(first frame|last frame|reference images|reference videos|reference audio) → (\S+)( \(only the first of (\d+)\))?$`, func(m []string, _ func(string) string) string {
		out := zhRoles[m[1]] + " → " + m[2]
		if m[4] != "" {
			out += "（共 " + m[4] + " 个，只发送第一个）"
		}
		return out
	}),
	pat(`^(first frame|last frame|reference images|reference videos|reference audio): not supported by this model, ignored$`, func(m []string, _ func(string) string) string {
		return zhRoles[m[1]] + "：该模型不支持，已忽略"
	}),
	pat(`^duration → (\S+) = (.+)$`, func(m []string, _ func(string) string) string { return "时长 → " + m[1] + " = " + m[2] }),
	pat(`^aspect ratio: (\S+) is not offered by this model, its default is used$`, func(m []string, _ func(string) string) string {
		return "画幅比例：该模型不提供 " + m[1] + "，使用其默认值"
	}),
	pat(`^The reference files total (\d+) MB and are sent inline; a request this large can be slow or be rejected\.$`, func(m []string, _ func(string) string) string {
		return "参考文件共 " + m[1] + " MB，以内联方式发送；这么大的请求可能很慢或被拒绝。"
	}),
	pat(`^shot (\d+) has no motion prompt yet: run step 5 or write one$`, func(m []string, _ func(string) string) string {
		return "镜头 " + m[1] + " 还没有运动提示词：请运行第 5 步或手动填写"
	}),
	pat(`^"(.+)" is not a configured image or video provider$`, func(m []string, _ func(string) string) string {
		return "“" + m[1] + "” 不是已配置的图像或视频服务商"
	}),
	pat(`^this list takes (image|video|audio) files$`, func(m []string, _ func(string) string) string {
		return "这个列表只接受" + map[string]string{"image": "图片", "video": "视频", "audio": "音频"}[m[1]] + "文件"
	}),
	pat(`^Shot (\d+)$`, func(m []string, _ func(string) string) string { return "镜头 " + m[1] }),
	pat(`^Could not write the request log: (.*)$`, func(m []string, _ func(string) string) string {
		return "无法写入请求日志：" + m[1]
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
	"done", "finished", "Could not save your edits: ",
	"No model list yet. Fetch it from the provider first.", "No models match.",
	"Showing the first 300; type to narrow down.", "All categories", "Refresh",
	"Select a provider first.", "Fetch model list", "Fetching…", "models", "Updated",
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
	"Video clips":        "视频片段",
	"Generate all clips": "生成全部片段",
	"Generate clip":      "生成片段",
	"Clips":              "片段",
	"Each clip opens on the shot's keyframe in use and follows its motion prompt. A last frame, reference videos and reference audio are sent if the model takes them; the run log says what was used.": "每个片段以该镜头「使用中」的关键帧开场，并按运动提示词生成。如果模型支持，还会发送尾帧、参考视频和参考音频；运行日志会说明实际用了哪些。",
	"No motion prompt yet: run step 5.": "还没有运动提示词：请运行第 5 步。",
	"First frame":                       "首帧",
	"Last frame":                        "尾帧",
	"Reference videos":                  "参考视频",
	"Reference audio":                   "参考音频",
	"Reference images":                  "参考图",
	"No keyframe: run step 6, or the clip is generated from the prompt and references alone.":          "没有关键帧：请运行第 6 步，否则片段只会根据提示词和参考素材生成。",
	"None: the model decides how the shot ends.":                                                       "无：由模型决定镜头如何结束。",
	"End this clip on the first frame of the next shot":                                                "让这个片段结束在下一个镜头的首帧上",
	"use the next shot's keyframe":                                                                     "使用下一个镜头的关键帧",
	"same as the keyframe's (step 6)":                                                                  "与关键帧相同（第 6 步）",
	"Use this one downstream":                                                                          "后续步骤使用这一个",
	"An earlier request is unfinished. Generating again collects its result instead of starting over.": "上一次请求还没有取回结果。再次生成会直接取回它，不会重新开始。",
	"Discard the unfinished request? Its result will be lost.":                                         "放弃这个未完成的请求？它的结果将丢失。",
	"discard": "放弃",
	"(English, the prompt of the video clip in step 7)": "（英文，第 7 步视频片段的提示词）",
	"Video generation": "视频生成",
	"Video model":      "视频模型",
	"(JSON object merged into every request, e.g. {\"resolution\": \"1080p\"})": "（合并到每次请求的 JSON 对象，例如 {\"resolution\": \"1080p\"}）",
	"Input mapping override": "输入映射覆盖",
	"(optional JSON, role → field name, for this model only; \"-\" switches a role off)": "（可选 JSON，角色 → 字段名，仅对该模型生效；填 \"-\" 表示不发送该项）",
	"How this model's inputs are used": "该模型的输入如何对应",
	"Detected from the model's published input list. Inputs the model lacks are skipped and reported in the run log.":                                              "根据模型公布的输入列表自动识别。模型不支持的输入会被跳过，并在运行日志中说明。",
	"The model's input list has not been fetched yet, so only the prompt and the first frame would be sent. It is fetched automatically on the first run, or now:": "还没有拉取该模型的输入列表，此时只会发送提示词和首帧。首次运行时会自动拉取，也可以现在拉取：",
	"Model field":                "模型字段",
	"Accepts":                    "可接受的值",
	"not supported":              "不支持",
	"Other inputs of this model": "该模型的其他输入",
	"Set any of these through Extra parameters.": "可以通过「附加参数」设置这些输入。",
	"Refresh input list":                         "刷新输入列表",
	"Fetch input list":                           "拉取输入列表",
	"Media type":                                 "媒体类型",
	"Any media":                                  "全部媒体",
	"Audio":                                      "音频",
	"Input files":                                "输入文件",
	"no video provider configured; set one in Settings":                                    "还没有配置视频服务商，请在设置中选择",
	"no video model configured; set one in Settings":                                       "还没有配置视频模型，请在设置中选择",
	"the model returned no video":                                                          "模型没有返回视频",
	"This shot has no keyframe: generating without a first frame.":                         "这个镜头没有关键帧：将在没有首帧的情况下生成。",
	"The request changed since the unfinished one was submitted: cancelling that one.":     "与之前未完成的请求相比内容已改变：正在取消旧请求。",
	"The request is still with the provider. Run this step again to collect its result.":   "请求仍在服务商处处理。再次运行此步骤即可取回结果。",
	"The model's parameter list is unknown: only the prompt and the first frame are sent.": "模型的参数列表未知：只发送提示词和首帧。",
	"Language models":           "语言模型",
	"Language model providers":  "语言模型服务商",
	"Image and video providers": "图像与视频服务商",
	"+ Add provider":            "+ 新增服务商",
	"API keys can be stored in the config file or read from an environment variable. Stored keys are never shown again.": "API 密钥可以保存在配置文件中，也可以从环境变量读取。已保存的密钥不会再显示。",
	"key stored":             "密钥已保存",
	"no key":                 "缺少密钥",
	"no key needed":          "无需密钥",
	"models":                 "个模型",
	"model list not fetched": "未拉取模型列表",
	"The model list has not been fetched yet.": "还没有拉取模型列表。",
	"Fetch model list":                         "拉取模型列表",
	"Delete":                                   "删除",
	"Cancel":                                   "取消",
	"Delete this provider? Steps that use it will have no provider.": "删除这个服务商？使用它的步骤将没有服务商。",
	"Fetching…":                             "拉取中…",
	"Choose from the provider's model list": "从服务商的模型列表中选择",
	"Browse":                                "选择",
	"Choose a model":                        "选择模型",
	"Category":                              "分类",
	"Search models":                         "搜索模型",
	"No model list yet. Fetch it from the provider first.": "还没有模型列表，请先从服务商拉取。",
	"No models match.": "没有匹配的模型。",
	"Showing the first 300; type to narrow down.": "仅显示前 300 个，请输入关键词缩小范围。",
	"All categories":                  "全部分类",
	"Refresh":                         "刷新",
	"Select a provider first.":        "请先选择服务商。",
	"provider not found":              "找不到该服务商",
	"the provider returned no models": "服务商没有返回任何模型",
	"Logs":                            "日志",
	"Request log":                     "请求日志",
	"← Request log":                   "← 请求日志",
	"Every LLM call and media generation is recorded as one JSON file, with the full prompt and output. API keys and image data are never stored.": "每次 LLM 调用和媒体生成都会记录为一个 JSON 文件，包含完整的提示词和输出。API 密钥和图片数据不会被保存。",
	"Days":                          "日期",
	"Nothing recorded yet.":         "还没有记录。",
	"Total":                         "共",
	"Delete the whole request log?": "删除全部请求日志？",
	"clear all":                     "清空全部",
	"Status":                        "状态",
	"Any status":                    "全部状态",
	"ok":                            "成功",
	"request(s)":                    "条请求",
	"delete this day":               "删除当天",
	"Delete this day's log?":        "删除这一天的日志？",
	"Time":                          "时间",
	"Duration":                      "耗时",
	"← Newer":                       "← 较新",
	"Older →":                       "较早 →",
	"No requests match.":            "没有符合条件的请求。",
	"download JSON":                 "下载 JSON",
	"Request":                       "请求",
	"Response":                      "响应",
	"Input images":                  "输入图片",
	"Output":                        "输出",
	"No response was received.":     "没有收到响应。",
	"System prompt":                 "系统提示词",
	"User prompt":                   "用户提示词",
	"Prompt":                        "提示词",
	"Parameters":                    "参数",
	"Details":                       "详情",
	"Video":                         "视频",
	"Visual prompts":                "视觉提示词",
	"Concept images":                "概念图",
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
	"← Projects":   "← 项目",
	"Steps":        "步骤",
	"Visuals":      "视觉",
	"Storyboard":   "分镜",
	"Shot prompts": "镜头提示词",
	"From a one-line idea to story, concept art, storyboard and keyframes. Every step runs only when you click it.": "从一句话创意到故事、概念图、分镜和关键帧。每一步都只在你点击时运行。",
	"Deleting a project keeps its images in the asset library.":                                                     "删除项目后，其图片仍保留在素材库中。",
	"Could not save your edits: ":    "无法保存你的修改：",
	"upload too large (limit 64 MB)": "上传内容过大（上限 64 MB）",
	"Warning: the stream ended without a finish reason; the output may be incomplete.": "警告：输出流在没有结束标记的情况下中断，内容可能不完整。",
	"Input":        "输入",
	"Project name": "项目名称",
	"Aspect ratio": "画幅比例",
	"Core idea":    "核心创意",
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
