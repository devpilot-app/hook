package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// newTestPaths 构造带配置与 token 的临时状态目录。
func newTestPaths(t *testing.T, port int) (config.Paths, string) {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	cfg := config.Default()
	cfg.DaemonID = "dd_test"
	cfg.GatewayPort = port
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(paths.TokenPath(), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths, token
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHookRelaysDecision(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest"}}`))
	}))
	defer srv.Close()

	paths, token := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{"session_id":"s1"}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d，want 0", code)
	}
	if gotPath != "/v1/hook/claude" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if gotBody != `{"session_id":"s1"}` {
		t.Fatalf("请求体 = %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "hookSpecificOutput") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHookSilentOnNoDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte(`{"kind":"decision"}`))
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("204 不应输出任何内容，得到 %q", stdout.String())
	}
}

func TestHookSilentWhenDaemonDown(t *testing.T) {
	// 先占一个端口再释放，使请求必然失败。
	srv := httptest.NewServer(http.NotFoundHandler())
	port := portOf(t, srv.URL)
	srv.Close()

	paths, _ := newTestPaths(t, port)
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("daemon 不在时不应输出，得到 %q", stdout.String())
	}
}

func TestHookIgnoresUnknownAgent(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	if code := runHook([]string{"--dir", paths.Dir, "other"}, strings.NewReader(`{}`), &stdout, &stderr); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if hits != 0 {
		t.Fatalf("未知 agent 不应请求网关，hits = %d", hits)
	}
}

func TestHookMissingConfigSilent(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", dir, "codex"}, strings.NewReader(`{}`), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

func TestDoctorJSON(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			statusHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"queue_depth": 3, "pending_approvals": 1,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runDoctor([]string{"--dir", paths.Dir, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr=%s", code, stderr.String())
	}
	if statusHits != 1 {
		t.Fatalf("status 命中 %d 次", statusHits)
	}

	var report struct {
		OK     bool          `json:"ok"`
		Checks []checkResult `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("解析 JSON 失败: %v\n%s", err, stdout.String())
	}
	if !report.OK {
		t.Fatalf("期望 ok=true: %s", stdout.String())
	}
	byName := map[string]checkResult{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if got := byName["gateway"]; got.Status != "ok" || !strings.Contains(got.Detail, "queue_depth=3") {
		t.Fatalf("gateway 检查 = %+v", got)
	}
	if got := byName["pairing"]; got.Status != "warn" {
		t.Fatalf("pairing 检查 = %+v", got)
	}
	// 未设置 RelayURL 时不应发起网络请求。
	if got := byName["relay"]; got.Status != "warn" {
		t.Fatalf("relay 检查 = %+v", got)
	}
}

func TestDoctorCorruptConfigFails(t *testing.T) {
	dir := t.TempDir()
	paths := config.NewPaths(dir)
	if err := os.WriteFile(paths.ConfigPath(), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--dir", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("退出码 = %d，want 1", code)
	}
	if !strings.Contains(stdout.String(), "config") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// TestHookClientTimeoutDerivation 锁住超时派生关系:
// 客户端超时(+50)必须严格小于 install 写入的 hook timeout(+60),
// daemon 的空 204 才能先于 Agent 侧超时到达。
func TestHookClientTimeoutDerivation(t *testing.T) {
	cfg := config.Default()
	if cfg.ApprovalTimeout != config.DefaultApprovalTimeoutSeconds {
		t.Fatalf("默认等待窗 = %d, want %d", cfg.ApprovalTimeout, config.DefaultApprovalTimeoutSeconds)
	}
	if d := hookClientTimeout(cfg); d != time.Duration(cfg.ApprovalTimeout+50)*time.Second {
		t.Fatalf("默认派生 = %v", d)
	}

	cfg.ApprovalTimeout = 60 // 用户自定义短窗同样满足约束
	d := hookClientTimeout(cfg)
	installTimeout := time.Duration(cfg.ApprovalTimeout+60) * time.Second
	if d >= installTimeout {
		t.Fatalf("客户端超时 %v 必须小于 install timeout %v", d, installTimeout)
	}
}

// fakeTmux 在 PATH 前插一个记录调用次数的 tmux 桩,返回计数文件路径。
// 同时放宽探测超时:整套 -race 并行时 fork/exec 可能被 CPU 饿超过生产 2s 兜底。
func fakeTmux(t *testing.T) string {
	t.Helper()
	oldTimeout := tmuxProbeTimeout
	tmuxProbeTimeout = 10 * time.Second
	t.Cleanup(func() { tmuxProbeTimeout = oldTimeout })

	bin := t.TempDir()
	counter := filepath.Join(bin, "calls")
	script := filepath.Join(bin, "tmux")
	// 输出带空白,顺带验证 TrimSpace;会话名 + 窗口号同取(v0.3.1)。
	// 每次调用追加一行到计数文件。
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ' main 2 '\necho x >> "+counter+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	return counter
}

func tmuxCalls(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(bytes.Split(data, []byte("\n"))) - 1
}

func TestHookTmuxHeaderInjected(t *testing.T) {
	counter := fakeTmux(t)
	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest"}`), &stdout, &stderr)

	if gotSession != "main" {
		t.Fatalf("tmux session header = %q, want main", gotSession)
	}
	if gotWindow != "2" {
		t.Fatalf("tmux window header = %q, want 2", gotWindow)
	}
	// 探测次数 1~2:有控制 tty 祖先时先 list-panes 锚定,桩无 tab 输出
	// 不命中再回落 display-message;无 tty 祖先时只走 display-message。
	// 合同是「有限次数」,两环境都不得越界。
	if n := tmuxCalls(t, counter); n < 1 || n > 2 {
		t.Fatalf("tmux 探测次数 = %d, want 1~2", n)
	}
}

// TestParseTmuxInfo 解析规则:会话名可含空格——行尾最后一个空格分隔
// token 为窗口号(全数字校验),其余全量保留。
func TestParseTmuxInfo(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		session string
		window  string
	}{
		{"普通", "main 2", "main", "2"},
		{"会话名含空格", "my session 3", "my session", "3"},
		{"带首尾空白", "  dev session 12  ", "dev session", "12"},
		{"窗口非数字置空(余下段保留)", "main x", "main", ""},
		{"无 token 视作纯会话名", "solo", "solo", ""},
		{"空行", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTmuxInfo(tc.line)
			if got.session != tc.session || got.window != tc.window {
				t.Fatalf("parseTmuxInfo(%q) = {%q %q}, want {%q %q}",
					tc.line, got.session, got.window, tc.session, tc.window)
			}
		})
	}
}

// TestParsePaneList list-panes -a 输出按 pane tty 精确匹配:
// 会话名可含空格、行尾 token 为窗口号; tty 归一化(/dev/ 前缀、"?"/"??" 不算)。
func TestParsePaneList(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		tty     string
		session string
		window  string
	}{
		{"命中窗口0", "/dev/ttys019\tDevPilot\t0\ndev/null\n", "ttys019", "DevPilot", "0"},
		{"多行选对 pane", "/dev/ttys001\t0\t0\n/dev/ttys019\tmy work\t3\n/dev/ttys010\tDevPilot\t1\n", "ttys019", "my work", "3"},
		{"无 /dev 前缀也命中", "ttys011\tbug\t2\n", "ttys011", "bug", "2"},
		{"tty 不匹配落空", "/dev/ttys001\tmain\t5\n", "ttys999", "", ""},
		{"无 tty 行不算命中", "?\tmain\t5\n??\tmain\t6\n", "?", "", ""},
		{"窗口非数字置空会话保留", "/dev/ttys019\tmain\tx\n", "ttys019", "main", ""},
		{"缺字段行跳过", "onlytty\n", "onlytty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePaneList(tc.out, tc.tty)
			if got.session != tc.session || got.window != tc.window {
				t.Fatalf("parsePaneList(tty=%q) = {%q %q}, want {%q %q}",
					tc.tty, got.session, got.window, tc.session, tc.window)
			}
		})
	}
}

// TestHookTmuxPaneTTYPreferred 有控制 tty 的环境下,窗口号取 agent pane
// 真位置(list-panes 精确锚定),而非 display-message 回落的「会话当前
// 激活窗」。桩让两条路径输出不同窗口号,断言取的是 pane 那份。
func TestHookTmuxPaneTTYPreferred(t *testing.T) {
	tty := ancestorTTY()
	if tty == "" {
		t.Skip("测试进程自身无控制 tty(如 CI),tty 精确锚定无从验证")
	}

	bin := t.TempDir()
	script := filepath.Join(bin, "tmux")
	// list-panes:真 pane(ttys 命中本测试 tty,窗口 2)+ 两行 decoy;
	// display-message:激活窗回落,故意输出不同窗口号 9。
	// decoy 的 ttys 由真 tty 加字母后缀派生,绝不与本进程 tty 撞名——
	// 早期硬编码 ttys001/ttys010,测试跑在新终端的首个 pty(ttys001)时
	// 诱饵行与真行双重命中,parsePaneList 取到诱饵(2026-11-16 本机复现)。
	stub := "#!/bin/sh\ncase \"$*\" in\n" +
		"*list-panes*)" +
		"printf '/dev/" + tty + "A\\t0\\t0\\n/dev/" + tty + "\\tmain\\t2\\n/dev/" + tty + "B\\tmain\\t9\\n';;" +
		"*)echo ' main 9 ';;\nesac\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")

	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`), &stdout, &stderr)

	if gotSession != "main" {
		t.Fatalf("tmux session header = %q, want main", gotSession)
	}
	if gotWindow != "2" {
		t.Fatalf("tmux window header = %q, want 2(pane 真位置,非激活窗 9)", gotWindow)
	}
}

// TestHookTmuxProbeEventWhitelist 探测名单外的事件不探测:
// 高频工具 hook 不能被 tmux 服务端卡死拖累;Stop(回合结束 relay)
// 低频且收件箱路由需要,在名单内。
func TestHookTmuxProbeEventWhitelist(t *testing.T) {
	cases := []struct {
		name      string
		eventName string
		mode      string // 空 = 默认 permission-request
		probe     bool
	}{
		{"permission_request 默认模式", "PermissionRequest", "", true},
		{"stop 回合结束 relay", "Stop", "", true},
		{"pre_tool_use 审批模式", "PreToolUse", string(config.ApprovalModePreToolUse), true},
		{"post_tool_use 不探测", "PostToolUse", "", false},
		{"pre_tool_use 非审批模式不探测", "PreToolUse", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := fakeTmux(t)
			var session, window string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				session = r.Header.Get("X-DevPilot-Tmux-Session")
				window = r.Header.Get("X-DevPilot-Tmux-Window")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			paths, _ := newTestPaths(t, portOf(t, srv.URL))
			if tc.mode != "" {
				cfg := config.Default()
				cfg.GatewayPort = portOf(t, srv.URL)
				cfg.ApprovalMode = tc.mode
				if err := cfg.Save(paths); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			body := `{"session_id":"s1","hook_event_name":"` + tc.eventName + `"}`
			runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(body), &stdout, &stderr)

			if tc.probe {
				if session != "main" {
					t.Fatalf("session header = %q, want main", session)
				}
				if window != "2" {
					t.Fatalf("window header = %q, want 2", window)
				}
			} else {
				if session != "" || window != "" {
					t.Fatalf("不应带 tmux header,得到 session=%q window=%q", session, window)
				}
				if n := tmuxCalls(t, counter); n != 0 {
					t.Fatalf("不应探测 tmux,实际 %d 次", n)
				}
			}
		})
	}
}

// TestHookTmuxEnvMissingStaysConservative 无 $TMUX(引擎被后台化/非 tmux
// 环境):绝不走 display-message——无身份时它报「最近活跃会话」,纯瞎猜;
// 只允许 cwd 深探锚(list-panes)。桩输出无 tab(探不出归属)→ 不带
// header,行为与旧「跳过探测」对外一致,但探测面已扩展。
func TestHookTmuxEnvMissingStaysConservative(t *testing.T) {
	counter := fakeTmux(t)
	t.Setenv("TMUX", "")
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-DevPilot-Tmux-Session")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest"}`), &stdout, &stderr)

	if header != "" {
		t.Fatalf("探不出归属不应带 header,得到 %q", header)
	}
	// list-panes(tty 锚或 cwd 深探)跑了;display-message 一次都不能有。
	// fakeTmux 的桩对所有子命令同输出,只能数总量:上限 2(tty 锚 + 深探),
	// display-message 若被调总量会到 3+。
	if n := tmuxCalls(t, counter); n < 1 || n > 2 {
		t.Fatalf("无 TMUX 探测次数 = %d, want 1~2(仅 list-panes)", n)
	}
}

// TestHookTmuxProjectProbeWhenEngineDetached 后台化引擎($TMUX 空、无 tty
// 祖先)的 cwd 深探:同项目 tmux pane 里跑着同 agent 的接入进程 → 按卡上
// 的 session/window 恢复归属;display-message 绝不被调。
func TestHookTmuxProjectProbeWhenEngineDetached(t *testing.T) {
	bin := t.TempDir()
	counter := filepath.Join(bin, "calls")
	tmuxStub := "#!/bin/sh\necho \"$*\" >> " + counter + "\n" +
		"case \"$*\" in\n" +
		"*list-panes*)" +
		"printf 'other\\t8\\t/elsewhere\\t1\\t1\\t99\\t99999\\n" +
		"1\\t2\\t/tmp/proj-x\\t1\\t1\\t50\\t4242\\n';;" +
		"*display-message*)echo 'WRONG 9';;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmuxStub), 0o755); err != nil {
		t.Fatal(err)
	}
	// ps 终验桩:全表形态——4242 = pane 外壳 zsh,孩子 4243 = claude
	// (子树命中路径);其余子树无 claude。
	psStub := "#!/bin/sh\ncat <<'EOF'\n" +
		"  4242       1 -zsh\n" +
		"  4243    4242 claude\n" +
		"  99999       1 -zsh\n" +
		"EOF\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(psStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")

	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop","cwd":"/tmp/proj-x"}`),
		&stdout, &stderr)

	if gotSession != "1" {
		t.Fatalf("tmux session header = %q, want 1(cwd 深探命中)", gotSession)
	}
	if gotWindow != "2" {
		t.Fatalf("tmux window header = %q, want 2", gotWindow)
	}
	for _, line := range readLines(t, counter) {
		if strings.Contains(line, "display-message") {
			t.Fatalf("无 TMUX 绝不应走 display-message,实际调用 %q", line)
		}
	}
}

// TestHookTmuxProjectProbeSessionIDHit session id 精确归属:同项目两个
// pane 同分并列(旧收敛规则会放弃),但其中一个 pane 的子树跑着
// `claude attach <session id>` → 唯一命中,不再依赖权重猜。
func TestHookTmuxProjectProbeSessionIDHit(t *testing.T) {
	const sid = "cc07dd44-94e3-40d3-8e1a-5119769ecf6b"
	bin := t.TempDir()
	tmuxStub := "#!/bin/sh\ncase \"$*\" in\n" +
		"*list-panes*)" +
		"printf '1\\t2\\t/tmp/proj-x\\t1\\t0\\t50\\t4242\\n" +
		"DevPilot\\t1\\t/tmp/proj-x\\t1\\t0\\t50\\t4244\\n';;" +
		"*display-message*)echo 'WRONG 9';;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmuxStub), 0o755); err != nil {
		t.Fatal(err)
	}
	psStub := "#!/bin/sh\ncat <<'EOF'\n" +
		"  4242       1 -zsh\n" +
		"  4243    4242 claude\n" +
		"  4244       1 -zsh\n" +
		"  4245    4244 claude attach " + sid + "\n" +
		"EOF\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(psStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")

	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"`+sid+`","hook_event_name":"Stop","cwd":"/tmp/proj-x"}`),
		&stdout, &stderr)

	if gotSession != "DevPilot" || gotWindow != "1" {
		t.Fatalf("session/window header = %q:%q, want DevPilot:1(session id 精确归属)", gotSession, gotWindow)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestParsePaneProjectRows cwd 预筛(纯函数):/private 归一、残缺行跳过、
// 窗口号非数字保留会话名、收敛字段落位。
func TestParsePaneProjectRows(t *testing.T) {
	cases := []struct {
		name string
		out  string
		cwd  string
		want []paneProjectRow
	}{
		{"单命中", "main\t2\t/tmp/p\t1\t1\t50\t4242\n", "/tmp/p",
			[]paneProjectRow{{info: tmuxInfo{session: "main", window: "2"}, active: true, attached: true, activity: 50, pid: "4242"}}},
		{"macOS realpath 归一", "main\t2\t/private/tmp/p/\t0\t0\t9\t1\n", "/tmp/p",
			[]paneProjectRow{{info: tmuxInfo{session: "main", window: "2"}, activity: 9, pid: "1"}}},
		{"cwd 不匹配不命中", "main\t2\t/elsewhere\t1\t1\t50\t7\n", "/tmp/p", nil},
		{"cwd 线索缺位不猜", "main\t2\t/tmp/p\t1\t1\t50\t7\n", "", nil},
		{"attached 零值判否", "main\t2\t/tmp/p\t0\t0\t50\t7\n", "/tmp/p",
			[]paneProjectRow{{info: tmuxInfo{session: "main", window: "2"}, activity: 50, pid: "7"}}},
		{"残缺行跳过其余照常", "garbage-no-tabs\ns\t1\t/tmp/p\t0\t0\t10\t3\n", "/tmp/p",
			[]paneProjectRow{{info: tmuxInfo{session: "s", window: "1"}, activity: 10, pid: "3"}}},
		{"窗口号非数字保留会话名", "s\txx\t/tmp/p\t1\t1\t50\t7\n", "/tmp/p",
			[]paneProjectRow{{info: tmuxInfo{session: "s"}, active: true, attached: true, activity: 50, pid: "7"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePaneProjectRows(tc.out, tc.cwd)
			if len(got) != len(tc.want) {
				t.Fatalf("候选数 = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("候选[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestConvergePaneRows 收敛规则(纯函数):active > attached > activity
// 三级排序,仍并列放弃(错路由比缺线索糟)。
func TestConvergePaneRows(t *testing.T) {
	row := func(session, window string, active, attached bool, activity int64) paneProjectRow {
		return paneProjectRow{info: tmuxInfo{session: session, window: window}, active: active, attached: attached, activity: activity}
	}
	cases := []struct {
		name    string
		rows    []paneProjectRow
		session string
		window  string
	}{
		{"空候选放弃", nil, "", ""},
		{"单候选直取", []paneProjectRow{row("main", "2", false, false, 1)}, "main", "2"},
		{"active 胜 attached", []paneProjectRow{row("a", "0", false, true, 50), row("b", "3", true, false, 9)}, "b", "3"},
		{"attached 胜 activity", []paneProjectRow{row("a", "0", false, true, 99), row("b", "3", false, false, 50)}, "a", "0"},
		{"activity 决胜同分", []paneProjectRow{row("a", "0", false, true, 10), row("b", "3", false, true, 99)}, "b", "3"},
		{"全同分放弃", []paneProjectRow{row("a", "0", false, true, 50), row("b", "3", false, true, 50)}, "", ""},
		{"双 active 同 activity 并列放弃", []paneProjectRow{row("a", "0", true, true, 50), row("b", "3", true, true, 50)}, "", ""},
		{"三候选唯一最优", []paneProjectRow{row("x", "0", true, true, 10), row("y", "1", true, true, 99), row("z", "2", false, true, 500)}, "y", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convergePaneRows(tc.rows)
			if got.session != tc.session || got.window != tc.window {
				t.Fatalf("convergePaneRows = %+v, want session=%q window=%q", got, tc.session, tc.window)
			}
		})
	}
}

// TestPaneSubtreeMatch ps 子树终验:pane_pid 是外壳 zsh,agent 在后代;
// argv 全文含 session id = 本会话 attach 视图(最强);argv0 basename
// 精确等值,前缀子串不误中;短 session id 不参与 argv 匹配;限深 4 层。
func TestPaneSubtreeMatch(t *testing.T) {
	bin := t.TempDir()
	// 全表:100=外壳 zsh + 孩子 200 claude attach cc07dd44(session 命中);
	// 300 链式 zsh→login→zsh→claude --resume x(仅 agent);700 链深 5 越限;
	// 900=claude-router 前缀子串;99=zombie。
	psStub := "#!/bin/sh\ncat <<'EOF'\n" +
		"  100     1 -zsh\n" +
		"  200   100 claude attach cc07dd44-94e3-40d3-8e1a-5119769ecf6b\n" +
		"  300     1 -zsh\n" +
		"  400   300 login -fp eric\n" +
		"  500   400 -zsh\n" +
		"  600   500 claude --resume x\n" +
		"  700     1 -zsh\n" +
		"  705   700 a\n" +
		"  710   705 b\n" +
		"  715   710 c\n" +
		"  720   715 d\n" +
		"  725   720 claude\n" +
		"  900     1 claude-router serve\n" +
		"   99     1 (zombie)\n" +
		"EOF\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(psStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	for _, tc := range []struct {
		pid, agent, sessionID string
		want       subtreeMatch
		note       string
	}{
		{"100", "claude", "cc07dd44-94e3-40d3-8e1a-5119769ecf6b", subtreeSession, "argv 含 session id = attach 视图"},
		{"300", "claude", "cc07dd44-94e3-40d3-8e1a-5119769ecf6b", subtreeAgent, "agent 在但无会话证据"},
		{"300", "claude", "", subtreeAgent, "session id 缺位降级 agent 判定"},
		{"300", "claude", "s1", subtreeAgent, "短串不参与 argv 匹配(防误撞)"},
		{"700", "claude", "", subtreeNone, "深 5 越限"},
		{"900", "claude", "", subtreeNone, "根自身前缀子串不误中"},
		{"99", "claude", "", subtreeNone, "zombie 无 argv0"},
		{"4242", "claude", "", subtreeNone, "表外 pid 淘汰"},
		{"", "claude", "", subtreeNone, "pid 缺位"},
		{"abc", "claude", "", subtreeNone, "非数字 pid"},
	} {
		if got := paneSubtreeMatch(ctx, tc.pid, tc.agent, tc.sessionID); got != tc.want {
			t.Fatalf("%s: paneSubtreeMatch(%q,%q) = %v, want %v", tc.note, tc.pid, tc.sessionID, got, tc.want)
		}
	}
}
