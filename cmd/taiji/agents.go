package main

import (
	"fmt"
	"os"
	"strings"
)

// envAgentsKey 是多 agent 配置的环境变量名。
//
// 单个键、值内含全部 agent（分号分隔条目）——与既有 TAIJI_RBAC 同形态
// （见 envRBAC）。额外收益：凭据保护面是**单个键**，无需前缀匹配
// （对比 TAIJI_AGENT_<name>_APP_ID 的形态需加 ReservedPrefixes）。
const envAgentsKey = "TAIJI_AGENTS"

// agentSpec 是一个 agent 的配置（名 + 凭据 + 隔离参数）。
type agentSpec struct {
	// Name 是 agent 标识，用于日志与执行层选择。
	Name string
	// AppID / AppSecret 是该 agent 绑定的飞书应用凭据（ak/sk）。
	//
	// 多 bot 部署下每个 agent 一套——入站按事件头的 Header.AppID 分流，
	// 出站按同一 agent 选凭据（形成闭环）。
	AppID     string
	AppSecret string

	// ── 隔离参数（全部可选，留空则用全局默认）──

	// Parent 是父 agent 名（N1 父子关系）。空表示顶层 agent。
	Parent string
	// Instruction 是该 agent 的系统提示（N2）。空则用 TAIJI_INSTRUCTION。
	Instruction string
	// Skills 是该 agent 的 skill 仓库根（N3）。空则用 TAIJI_SKILLS_ROOT。
	Skills string
	// AllowTools 是该 agent 的 MCP 工具白名单（N4）。
	// 空则用 TAIJI_ALLOW_TOOLS。
	//
	// 值用 `|` 分隔而非逗号——逗号已是字段分隔符，再用会歧义。
	AllowTools []string
}

// parseAgents 解析 TAIJI_AGENTS（agent 配置）。
//
// 格式：分号分隔 agent，逗号分隔字段（name / app_id / app_secret）：
//
//	单 agent： TAIJI_AGENTS="name=assistant,app_id=cli_aaa,app_secret=s1"
//	多 agent： TAIJI_AGENTS="name=root,app_id=cli_aaa,app_secret=s1;name=bill,app_id=cli_bbb,app_secret=s2"
//
// **单 agent 与多 agent 是同一个配置面**——区别只在条目数。
// 下游按 `len(specs) > 1` 判别（见 buildAgents），故配一条即单 agent，
// 无需另一套变量。
//
// 未配置即返回 error（2026-10-02 起）——此前回退读 FEISHU_APP_ID /
// FEISHU_APP_SECRET，那两个变量已删除，配置面收敛到本变量一条通道。
// 缺配置时**明确报错**而非静默用空凭据启动：后者会在连飞书时才失败，
// 且错误指向「app_id is invalid」而非「你没配配置」，误导排查方向。
//
// 配置有误即返回 error（fail-fast）——错误信息**指出是哪个 agent**，
// 否则多 agent 下用户不知道哪条写错了。与 envRBAC 的取向一致：
// 「配了却不生效」类静默失效最难排查。
func parseAgents() ([]agentSpec, error) {
	raw := strings.TrimSpace(os.Getenv(envAgentsKey))
	if raw == "" {
		return nil, fmt.Errorf(
			"%s 未配置——它是 agent 与飞书凭据的**唯一**配置入口。"+
				"单 agent 写法：%s=\"name=assistant,app_id=<你的app_id>,app_secret=<你的app_secret>\"",
			envAgentsKey, envAgentsKey)
	}

	var specs []agentSpec
	seenName := make(map[string]bool)
	seenAppID := make(map[string]bool)

	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue // 容忍尾部空条目
		}

		spec, err := parseAgentEntry(entry)
		if err != nil {
			return nil, err
		}

		// 重名检查：同一 agent 名出现两次会让执行层选择不确定。
		if seenName[spec.Name] {
			return nil, fmt.Errorf(
				"%s 配置错误：agent 名 %q 重复——"+
					"会导致执行层选择歧义", envAgentsKey, spec.Name)
		}
		seenName[spec.Name] = true

		// app_id 跨 agent 重复：同一条消息（Header.AppID）会命中两个 agent，
		// 分流结果不确定。
		if seenAppID[spec.AppID] {
			return nil, fmt.Errorf(
				"%s 配置错误：app_id %q 被多个 agent 使用——"+
					"同一应用的消息无法确定归属", envAgentsKey, spec.AppID)
		}
		seenAppID[spec.AppID] = true

		specs = append(specs, spec)
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("%s 配置错误：未解析出任何 agent", envAgentsKey)
	}

	// 父子关系的完整性校验（N1）。
	//
	// 两项必查，缺一不可：
	//  1. 父必须存在——否则该子永远无法被 transfer 到（静默失效）。
	//  2. 不得成环——拓扑序装配会无限递归；且 FindSubAgent 语义未定义。
	if err := validateParents(specs); err != nil {
		return nil, err
	}

	return specs, nil
}

// validateParents 校验父子声明的引用完整性与无环性。
func validateParents(specs []agentSpec) error {
	byName := make(map[string]agentSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	// 1) 父必须存在。
	for _, s := range specs {
		if s.Parent == "" {
			continue
		}
		if _, ok := byName[s.Parent]; !ok {
			return fmt.Errorf(
				"%s 配置错误：agent %q 的 parent %q 未定义——"+
					"该子 agent 将永远无法被调用",
				envAgentsKey, s.Name, s.Parent)
		}
	}

	// 2) 无环（含自环）。用着色法：未访问/在栈上/已完成。
	const (
		unvisited = 0
		inStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(specs))
	var walk func(name string, path []string) error
	walk = func(name string, path []string) error {
		switch state[name] {
		case inStack:
			return fmt.Errorf(
				"%s 配置错误：父子关系成环（环路径：%v → %s）——"+
					"拓扑序装配会无限递归",
				envAgentsKey, path, name)
		case done:
			return nil
		}
		state[name] = inStack
		if p := byName[name].Parent; p != "" {
			if err := walk(p, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, s := range specs {
		if err := walk(s.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

// parseAgentEntry 解析单条 agent 定义（逗号分隔字段）。
func parseAgentEntry(entry string) (agentSpec, error) {
	var spec agentSpec
	for _, kv := range strings.Split(entry, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return agentSpec{}, fmt.Errorf(
				"%s 配置错误：条目 %q 中的 %q 不是 key=value 形式",
				envAgentsKey, entry, kv)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "name":
			spec.Name = v
		case "app_id":
			spec.AppID = v
		case "app_secret":
			spec.AppSecret = v
		case "parent":
			spec.Parent = v
		case "instruction":
			spec.Instruction = v
		case "skills":
			spec.Skills = v
		case "allow_tools":
			// `|` 分隔——逗号是字段分隔符，不能再用于列表。
			spec.AllowTools = splitPipeList(v)
		}
	}

	// 三字段都必填——缺任一项都无法建立「agent ↔ 飞书应用」的绑定。
	if spec.Name == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：条目 %q 缺少 name", envAgentsKey, entry)
	}
	if spec.AppID == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：agent %q 缺少 app_id", envAgentsKey, spec.Name)
	}
	if spec.AppSecret == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：agent %q 缺少 app_secret", envAgentsKey, spec.Name)
	}
	return spec, nil
}

// splitPipeList 按 `|` 切分并去空白，丢弃空项。
//
// 为什么不用逗号：逗号已是 agent 字段的分隔符（name=,app_id=,...），
// 再用于列表会让 `allow_tools=a,b` 与 `name=x,allow_tools=a,b` 歧义。
func splitPipeList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, "|") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
