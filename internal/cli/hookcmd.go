package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// tmuxProbeTimeout 是 tmux display-message 的兜底超时:
// 正常是本地 unix socket 往返(毫秒级),超时只为防 tmux 服务端卡死拖住 hook。
// 生产固定 2s;提成 var 仅供测试在整套 -race 并行、进程启动被饿时放宽。
var tmuxProbeTimeout = 2 * time.Second

// hookClientTimeout 由审批等待窗派生:+50s,且必须小于 install 写入的
// hook timeout(+60s)——保证 daemon 的「无决策」空响应先于 Agent 侧
// hook 超时到达,Claude Code 回落原生询问而不是报 hook 失败。
func hookClientTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.ApprovalTimeout+50) * time.Second
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	pathsFlag(fs, &dir)
	if err := fs.Parse(args); err != nil {
		return 0 // 绝不因为参数问题破坏 Agent
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "claude" && fs.Arg(0) != "codex" && fs.Arg(0) != "opencode") {
		return 0
	}
	agentName := fs.Arg(0)

	paths, err := resolvePaths(dir)
	if err != nil {
		return 0
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return 0
	}
	token, err := os.ReadFile(paths.TokenPath())
	if err != nil {
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}

	base := statusURL(cfg)
	if override := os.Getenv("DEVPILOT_HOOK_GATEWAY"); override != "" {
		base = strings.TrimRight(override, "/")
	}
	// CLAUDE_PROJECT_DIR 是会话启动时的项目根，Claude Code 把它导出给 hook
	// 进程且整个会话不变；hook 输入里的 cwd 会跟随 Claude 的 `cd`（官方文档
	// 明示）。标题与会话归属以前者为准，所以每个事件都带给网关。
	projectDir := strings.TrimSpace(os.Getenv("CLAUDE_PROJECT_DIR"))
	ctx, cancel := context.WithTimeout(context.Background(), hookClientTimeout(cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/hook/"+agentName, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/json")
	if info := tmuxProbe(body, cfg, agentName, projectDir); info.session != "" {
		req.Header.Set("X-DevPilot-Tmux-Session", info.session)
		if info.window != "" {
			req.Header.Set("X-DevPilot-Tmux-Window", info.window)
		}
		if info.pane != "" {
			req.Header.Set("X-DevPilot-Tmux-Pane", info.pane)
		}
	}
	if projectDir != "" {
		req.Header.Set("X-DevPilot-Project-Dir", projectDir)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	// 只有明确的 200 才输出决策；其余一律无输出，让 Agent 走原生流程。
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(stdout, resp.Body)
	}
	return 0
}

// tmuxInfo 是一次探测取回的 tmux 定位:会话名 + 窗口号(v0.3.1) + 窗格 id。
type tmuxInfo struct {
	session string
	window  string
	// pane 是 agent 所在窗格的稳定 id（#{pane_id}，形如 %3）。窗口号只定位到
	// 窗口；同窗口多窗格时 App 据此把焦点落到对的窗格。老 tmux / 桩输出
	// 没有这一列时为空，App 回落「只到窗口」。
	pane string
}

// tmuxProbe 探测当前 tmux 会话名与窗口号(经专用 header 传给 daemon,由
// 网关盖进 payload,App 收件箱据此路由回会话/展示详情)。探测名单 =
// 需要 tmux 定位的事件:审批候选(PermissionRequest、pre-tool-use 审批
// 模式的 PreToolUse)+ 问题卡(PreToolUse 的 AskUserQuestion——它不走
// PermissionRequest 通道,是唯一纯导航卡,Answer at Terminal 全靠这组
// 坐标;permission-request 模式下普通 PreToolUse 遥测不探测,唯独问题卡
// 必须探:2026-11-22 实锤缺它导致卡上无坐标,App 只能按 cwd 派生名
// new -A 新开同名会话跑 resume fork,进错会话还展示「Interrupted」)+
// 会话启动(SessionStart,每会话一次:此刻 cwd 一定是项目根、进程树最
// 干净,是锚点最可信的一次观测,网关据此建会话锚点缓存)+ 回合结束
// (Stop→TaskComplete,回复摘要卡同要路由回 tmux 会话,否则收件箱把它当
// 非 tmux 会话展示)。问题卡每问题至多一次,与 Stop 同频级,不会拖累
// 高频工具链;PostToolUse 等遥测保持不探测——hook 调用频繁,无条件探测
// 会被卡死的 tmux 服务端拖累高频工具链。
//
// projectDir 是会话启动时的项目根(CLAUDE_PROJECT_DIR,可为空)。cwd 深探
// 用它做预筛:hook 输入的 cwd 会跟随 Claude 的 `cd`,拿它比对 pane 的
// 当前目录,agent 一跑 `cd 子目录` 之后同会话所有事件的锚点就全丢了
// (2026-11-23 真机:第二条审批标题漂成子目录名、tmux 锚点为空)。
//
// 定位分三级(2026-11-18 定型):
//  1. tty 精确锚:进程树向上找控制 tty 按 list-panes 钉 pane——hook 被
//     Agent 以 setsid 拉起,自身无控制 tty,Agent 进程自己持有 pane 的
//     控制 tty,是唯一无歧义锚点;
//  2. display-message 回落(仅 $TMUX 非空):$TMUX 由 tmux 服务端注入,
//     非空 = Agent 引擎在 tmux 内,会话名准、窗口号 best-effort(用户
//     此刻在看哪窗就报哪窗,并非 agent 所在窗)。$TMUX 空时绝不走这里
//     ——无身份时 display-message 报「最近活跃会话」,纯瞎猜;
//  3. cwd 深探锚($TMUX 空的主路径):引擎被后台化后(Claude Code
//     bg-pty-host 自管 PTY,$TMUX 空、无 tty 祖先)脱离 tmux 进程树,
//     但用户在 tmux pane 里打开同一会话时,pane 里跑着同 agent 的接入
//     进程——「pane_current_command == agent 且 pane_current_path ==
//     项目根」即可恢复归属(paneByProject)。全落空 = 真不在 tmux
//     附近,不带 header,网关按无会话处理(宁缺毋滥)。
func tmuxProbe(body []byte, cfg *config.Config, agentName, projectDir string) tmuxInfo {
	in, err := agent.ParseInput(body)
	if err != nil {
		return tmuxInfo{}
	}
	wantsTmuxProbe := in.HookEventName == "PermissionRequest" ||
		in.HookEventName == "SessionStart" ||
		in.HookEventName == "Stop" ||
		(in.HookEventName == "PreToolUse" &&
			(config.ApprovalMode(cfg.ApprovalMode) == config.ApprovalModePreToolUse ||
				in.ToolName == "AskUserQuestion"))
	if !wantsTmuxProbe {
		return tmuxInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxProbeTimeout)
	defer cancel()
	if tty := ancestorTTY(); tty != "" {
		if info := paneByTTY(ctx, tty); info.session != "" {
			return info
		}
	}
	if os.Getenv("TMUX") != "" {
		out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p",
			"#S #{window_index} #{pane_id}").Output()
		if err == nil {
			if info := parseTmuxDisplay(strings.TrimSpace(string(out))); info.session != "" {
				return info
			}
		}
	}
	in.ProjectDir = projectDir
	return paneByProject(ctx, agent.ProjectRoot(in), agentName, in.SessionID)
}

// ancestorTTY 沿进程树向上找最近一个带控制 tty 的祖先,返回归一化 tty
// 名(去 /dev/ 前缀,如 ttys019)。控制 tty 不随 setsid 子进程继承——
// Agent 拉起的 hook 自身必无 tty,而 Agent 交互进程有。上限 8 层,
// ps 失败或链尽返回空。
func ancestorTTY() string {
	pid := os.Getppid()
	for i := 0; i < 8 && pid > 1; i++ {
		out, err := exec.Command("ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return ""
		}
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			return ""
		}
		if tty := normalizeTTY(fields[len(fields)-1]); tty != "" {
			return tty
		}
		ppid, err := strconv.Atoi(fields[0])
		if err != nil {
			return ""
		}
		pid = ppid
	}
	return ""
}

// normalizeTTY 归一化 ps/tty 输出:去 /dev/ 前缀;无 tty 的 "?"/"??"
// 及空串返回空。
func normalizeTTY(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(s, "/dev/"))
	if s == "" || s == "?" || s == "??" {
		return ""
	}
	return s
}

// paneByTTY 在 list-panes -a 输出中按 pane tty 精确匹配,同取会话名、
// 窗口号与窗格 id——这是 agent 所在 pane 的真位置,与哪端正在看无关。
func paneByTTY(ctx context.Context, tty string) tmuxInfo {
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_tty}\t#{session_name}\t#{window_index}\t#{pane_id}").Output()
	if err != nil {
		return tmuxInfo{}
	}
	return parsePaneList(string(out), tty)
}

// parsePaneList 逐行匹配 pane_tty(第一个 \t 前的字段,ttys 不含空白);
// 第 2、3 段走 parseTmuxInfo(会话名可含空格,行尾 token 为窗口号),可选的
// 第 4 段是窗格 id(老 tmux / 旧桩输出缺这一列时窗格为空)。
func parsePaneList(out, tty string) tmuxInfo {
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 3 || normalizeTTY(parts[0]) != tty {
			continue
		}
		info := parseTmuxInfo(parts[1] + " " + parts[2])
		info.pane = paneIDColumn(parts, 3)
		return info
	}
	return tmuxInfo{}
}

// parseTmuxDisplay 解析 display-message -p "#S #{window_index} #{pane_id}"
// 的输出:行尾若是窗格 id(%N)先摘掉,余下交给 parseTmuxInfo。旧 tmux / 桩
// 没有 pane 段时同样适用。
func parseTmuxDisplay(line string) tmuxInfo {
	fields := strings.Fields(line)
	if n := len(fields); n > 1 && isPaneID(fields[n-1]) {
		info := parseTmuxInfo(strings.Join(fields[:n-1], " "))
		info.pane = fields[n-1]
		return info
	}
	return parseTmuxInfo(line)
}

// isPaneID 判窗格 id 形态:tmux 的 #{pane_id} 恒为 % 加数字(如 %3)。
func isPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// paneIDColumn 取分段里第 idx 列的窗格 id;列缺失或形态不符返回空。
func paneIDColumn(parts []string, idx int) string {
	if len(parts) <= idx {
		return ""
	}
	if id := strings.TrimSpace(parts[idx]); isPaneID(id) {
		return id
	}
	return ""
}

// paneByProject 按「项目根 + pane 前台进程」深探 tmux pane(第二锚,
// 三级探测的兜底级)。引擎被后台化(Claude Code bg-pty-host 自管 PTY,
// $TMUX 空)脱离 tmux 进程树后,tty 锚与 display-message 全部失效;但
// 用户在 tmux pane 里打开同一会话时,pane 里跑着同 agent 的接入进程,
// pane 的当前目录就是会话的项目根——按此恢复归属。projectRoot 必须传
// 会话启动时的项目根(CLAUDE_PROJECT_DIR),不是跟随 `cd` 的事件 cwd。
//
// 三段流水线:cwd 预筛(纯)→ ps 终验(pane 前台进程的 argv0 basename
// == agent)→ 收敛(纯)。两段纯逻辑拆出便于表驱动测试;ps 终验不可省
// ——pane_current_command 在 macOS tmux 上报的是可执行文件 basename
// (claude 装在 versions/2.1.325 时报 "2.1.325"),且同目录裸 zsh 也会
// 被 cwd 命中,只有 argv0 可靠。
func paneByProject(ctx context.Context, projectRoot, agent, sessionID string) tmuxInfo {
	target := normalizeProjPath(projectRoot)
	if target == "" || strings.TrimSpace(agent) == "" {
		return tmuxInfo{} // 线索缺位无从归属,不猜
	}
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{pane_current_path}\t#{window_active}\t#{session_attached}\t#{window_activity}\t#{pane_pid}\t#{pane_id}").Output()
	if err != nil {
		return tmuxInfo{}
	}
	candidates := parsePaneProjectRows(string(out), target)
	var sessionHits, agentHits []paneProjectRow
	for _, row := range candidates {
		switch paneSubtreeMatch(ctx, row.pid, agent, sessionID) {
		case subtreeSession:
			sessionHits = append(sessionHits, row)
		case subtreeAgent:
			agentHits = append(agentHits, row)
		}
	}
	// session id 命中 = 本会话的 attach 视图(claude attach <id>),唯一
	// 归属,直接取;理论上不会多命中(session id 全局唯一),防御性放弃。
	if len(sessionHits) == 1 {
		return sessionHits[0].info
	}
	if len(sessionHits) > 1 {
		return tmuxInfo{}
	}
	return convergePaneRows(agentHits)
}

// paneProjectRow 是深探探针的一行候选与收敛权重。
type paneProjectRow struct {
	info     tmuxInfo
	active   bool  // 所在窗口是该会话的激活窗口(用户视线所在)
	attached bool  // 会话有客户端挂载(终端开着 / DevPilot attach 中)
	activity int64 // 窗口最后活跃 epoch,越大越新
	pid      string
}

// parsePaneProjectRows 解析 list-panes 输出并按项目根预筛(纯函数,测试钉)。
// 行格式 8 列:session \t window \t path \t window_active \t attached
// \t activity \t pane_pid \t pane_id(末列可选)。session_name 禁含 `:`,
// `\t` 分列不受名字内空格影响;path 归一(/private 前缀/尾斜杠)后按
// sameProjectPath 比对。
func parsePaneProjectRows(out, cwd string) []paneProjectRow {
	target := normalizeProjPath(cwd)
	if target == "" {
		return nil
	}
	var rows []paneProjectRow
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.SplitN(line, "\t", 8)
		if len(parts) < 7 || parts[0] == "" {
			continue
		}
		if !sameProjectPath(parts[2], target) {
			continue
		}
		info := parseTmuxInfo(parts[0] + " " + parts[1])
		if info.session == "" {
			continue
		}
		info.pane = paneIDColumn(parts, 7)
		rows = append(rows, paneProjectRow{
			info:     info,
			active:   parts[3] == "1",
			attached: parts[4] != "0" && parts[4] != "",
			activity: parseProjEpoch(parts[5]),
			pid:      strings.TrimSpace(parts[6]),
		})
	}
	return rows
}

// sameProjectPath 判 pane 当前目录是否就是项目根 target:全等,或 pane 目录
// 位于项目根之下(按路径分量, /a/b 之下可以是 /a/b/c,不能是 /a/bb)。
// agent 从仓库子目录启动时项目根与 pane 目录差一到几层,全等比对会把整个
// 会话判成「不在 tmux」。反向(pane 目录是项目根的祖先)不算:那等于让任何
// 停在上层的 claude 进程冒领本会话。同项目多开的并列由收敛规则处理。
func sameProjectPath(panePath, target string) bool {
	p := normalizeProjPath(panePath)
	if p == target {
		return true
	}
	if target == "" || target == "." || len(p) <= len(target) {
		return false
	}
	prefix := target
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(p, prefix)
}

// paneSubtreeMaxDepth 是 pane_pid 进程子树下溯的深度上限。pane_pid 是
// pane 里最早的外壳进程(zsh),agent 是它的后代:zsh -c 'claude …' 挂
// 一层,登录链(login→zsh→claude)两三层;4 层已覆盖一切现实形态。
const paneSubtreeMaxDepth = 4

// sessionIDMinLen 是 session id 参与 argv 匹配的最短长度:真实形态
// (claude/codex 的 UUID v7、opencode 的 ses_+20)都远超此值;闸门防
// 短合成串(如测试用 "s1")在 argv 全文里到处误撞。
const sessionIDMinLen = 8

// subtreeMatch 是 pane 进程子树的终验结论。
type subtreeMatch int

const (
	subtreeNone   subtreeMatch = iota // 子树里没有 agent
	subtreeAgent                      // 有 agent,但无会话级证据
	subtreeSession                    // agent 进程 argv 含事件 session id = 本会话的 attach 视图,最强归属
)

// paneSubtreeMatch 终验 pane 的身份:pane_pid 是外壳(zsh),agent 跑在
// 它的进程子树里——ps 全表建 ppid→children 映射,从 pane_pid 限深下溯:
// 任一 agent 节点 argv 全文含 session id → subtreeSession(claude attach
// <id> 形态,唯一归属);仅 argv0 basename 等值 → subtreeAgent(防
// `claude-router` 前缀子串误中)。ps 失败 = 取证不可得 → 保守淘汰。
func paneSubtreeMatch(ctx context.Context, rootPID, agent, sessionID string) subtreeMatch {
	root, err := strconv.ParseInt(strings.TrimSpace(rootPID), 10, 64)
	if err != nil || root <= 0 {
		return subtreeNone
	}
	sessionID = strings.TrimSpace(sessionID)
	if len(sessionID) < sessionIDMinLen {
		sessionID = "" // 短串不做 argv 全文匹配
	}
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,args=").Output()
	if err != nil {
		return subtreeNone
	}
	argv0 := make(map[int64]string)
	fullArgs := make(map[int64]string)
	children := make(map[int64][]int64)
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue // 表头残留/zombie 无 args
		}
		pid, errPID := strconv.ParseInt(f[0], 10, 64)
		ppid, errPPID := strconv.ParseInt(f[1], 10, 64)
		if errPID != nil || errPPID != nil {
			continue
		}
		argv0[pid] = f[2]
		fullArgs[pid] = strings.Join(f[2:], " ")
		children[ppid] = append(children[ppid], pid)
	}
	type frame struct {
		pid   int64
		depth int
	}
	stack := []frame{{root, 0}}
	visited := map[int64]bool{root: true}
	isAgent := false
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.depth > paneSubtreeMaxDepth {
			continue
		}
		if filepath.Base(argv0[cur.pid]) == agent {
			isAgent = true
			if sessionID != "" && strings.Contains(fullArgs[cur.pid], sessionID) {
				return subtreeSession
			}
		}
		for _, child := range children[cur.pid] {
			if !visited[child] {
				visited[child] = true
				stack = append(stack, frame{child, cur.depth + 1})
			}
		}
	}
	if isAgent {
		return subtreeAgent
	}
	return subtreeNone
}

// convergePaneRows 收敛唯一归属(纯函数,测试钉):active > attached >
// activity 三级排序(用户正在看的 pane 最可信);仍并列 = 同项目多开
// 会话,归属不明 → 放弃(错路由比缺线索糟)。
func convergePaneRows(rows []paneProjectRow) tmuxInfo {
	if len(rows) == 0 {
		return tmuxInfo{}
	}
	best, tied := rows[0], false
	for _, row := range rows[1:] {
		switch comparePaneRows(row, best) {
		case 1:
			best, tied = row, false // 更优候选取代
		case 0:
			tied = true // 同分并列,归属不明
		}
	}
	if tied {
		return tmuxInfo{}
	}
	return best.info
}

// comparePaneRows 收敛权重:1 = a 优于 b,-1 = b 优,0 = 完全同分
// (window_active 2 分 > session_attached 1 分 > activity 数值)。
func comparePaneRows(a, b paneProjectRow) int {
	as, bs := paneScore(a), paneScore(b)
	if as != bs {
		if as > bs {
			return 1
		}
		return -1
	}
	switch {
	case a.activity > b.activity:
		return 1
	case a.activity < b.activity:
		return -1
	}
	return 0
}

func paneScore(r paneProjectRow) int {
	score := 0
	if r.active {
		score += 2
	}
	if r.attached {
		score += 1
	}
	return score
}

func parseProjEpoch(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// normalizeProjPath 归一路径比对:剥尾部斜杠与 /private 前缀——macOS
// tmux 的 pane_current_path 报 realpath(/private/tmp/…),而 agent 的
// cwd 常是 /tmp/… 符号链接形态,同一目录两边写法不一。
func normalizeProjPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/")
	if p == "/private" || strings.HasPrefix(p, "/private/") {
		p = strings.TrimPrefix(p, "/private")
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// parseTmuxInfo 解析 display-message 输出:会话名可含空格,不能天真
// Split——从行尾取最后一个空格分隔 token 为窗口号(全数字校验,非数字
// 置空),其余全量保留为会话名。
func parseTmuxInfo(line string) tmuxInfo {
	// 自防御 trim:调用方通常已 TrimSpace,但直接喂原始输出也不出错
	line = strings.TrimSpace(line)
	if line == "" {
		return tmuxInfo{}
	}
	idx := strings.LastIndexByte(line, ' ')
	if idx < 0 {
		// 只有一段:按旧协议视作纯会话名
		return tmuxInfo{session: line}
	}
	session := strings.TrimSpace(line[:idx])
	window := strings.TrimSpace(line[idx+1:])
	for _, r := range window {
		if r < '0' || r > '9' {
			window = "" // 非数字(旧 tmux/异常输出):窗口号置空,余下段保留为会话名
			break
		}
	}
	return tmuxInfo{session: session, window: window}
}
