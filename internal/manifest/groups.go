package manifest

import (
	"strings"
	"unicode"
)

// splitTrimGroups 按上游语义拆分组字符串：逗号与任意空白（空格/制表符/换行）
// 均可作分隔符（对齐上游 _ParseList 的 re.split(r"[,\s]+")），空项丢弃，
// 去重并保留首次出现顺序。
// 注意：上游不支持分号；分号会被视为组名字符的一部分，因此 "a;b" 是单个组名。
func splitTrimGroups(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// SplitGroups 以组过滤语义拆分字符串（逗号/空白分隔，丢弃空项与重复项）。
// 供命令行 -g/--groups 等调用方使用，与 manifest 内 groups 属性的解析保持同一套
// 分隔符规则（上游 _ParseList），避免出现"清单内支持空格分隔、命令行只认逗号"
// 的语义漂移。
func SplitGroups(s string) []string {
	return splitTrimGroups(s)
}

// dedupStrings 去重，保留首次出现的顺序。用于 EffectiveGroups 合并显式与隐式组后去重，
// 处理显式 name:<name> 与隐式 name:<name> 撞名等情况。
func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// EffectiveGroups 返回项目的有效组集合（综合并去重），对齐上游 repo 语义：
//   - 显式 groups 属性按逗号/空白拆分、去重（上游 _ParseSet/_ParseList）；
//   - 任何项目隐式归属 "all"（上游解析时 groups |= {"all"}）；
//   - 追加隐式 name:<name> 与 path:<path 或 name> 组（上游 _ParseProject）；
//   - "default" 不在此处并入：上游在过滤求值时才动态归属——任何不含
//     "notdefault" 组的项目都匹配 "default"，与是否显式声明 groups 无关；
//   - 合并后整体去重，保留首次出现顺序。
//
// 例如 groups="default,cix,soc"、name=p、path=p 时返回
// ["default","cix","soc","all","name:p","path:p"]。
func (p Project) EffectiveGroups() []string {
	groups := splitTrimGroups(p.Groups)

	// 所有项目隐式归属 all（上游在解析与过滤两端都并入 all）
	groups = append(groups, "all")

	// 隐式 name:<name>
	if p.Name != "" {
		groups = append(groups, "name:"+p.Name)
	}

	// 隐式 path:<path>；path 为空时回退到 name（Parse 阶段已保证 path 非空，此处防御）
	path := p.Path
	if path == "" {
		path = p.Name
	}
	if path != "" {
		groups = append(groups, "path:"+path)
	}

	// 显式与隐式合并后整体去重（处理显式 name:<name> 与隐式撞名等）
	return dedupStrings(groups)
}

// MatchesGroupFilter 判定项目是否匹配组过滤条件，对齐上游 Project.MatchesGroups：
//   - filter 为空表示不过滤（匹配全部）；
//   - 过滤项按出现顺序求值（"labels are resolved in order"）：
//     "-<group>" 命中时置否、"<group>" 命中时置真，最终结果由最后一次命中决定；
//   - "-<group>" 仅作用于过滤表达式（命令行 -g 等），manifest 内 groups 属性中的
//     "-" 无特殊含义；
//   - "all" 为项目隐式组，无需特判；"default" 匹配任何不含 notdefault 组的项目
//     （显式声明了其他 groups 不会剥夺 default 归属，见 EffectiveGroups）；
//   - name:/path: 隐式组同样参与匹配。
//
// 该方法是项目组判定的单一真相源；shouldIncludeProject 及 init/sync 的过滤
// 均应委托至此，避免多套实现语义漂移导致项目被错误保留或丢弃。
func (p Project) MatchesGroupFilter(filter []string) bool {
	if len(filter) == 0 {
		return true
	}

	// 候选集合 = 有效组（含隐式 all/name:/path:）∪ default（不含 notdefault 时）
	eff := p.EffectiveGroups()
	candidates := make(map[string]struct{}, len(eff)+1)
	for _, g := range eff {
		candidates[g] = struct{}{}
	}
	if _, hasNotDefault := candidates["notdefault"]; !hasNotDefault {
		candidates["default"] = struct{}{}
	}

	matched := false
	for _, g := range filter {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if strings.HasPrefix(g, "-") {
			if _, ok := candidates[strings.TrimPrefix(g, "-")]; ok {
				matched = false
			}
			continue
		}
		if _, ok := candidates[g]; ok {
			matched = true
		}
	}
	return matched
}
