package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-Hook/internal/agent"
)

// openCodeTestEnv 把 XDG_CONFIG_HOME 指到临时目录，并放一个假的
// `opencode` 可执行文件到 PATH 上，隔离真实用户路径与真实安装。
// 返回插件配置根目录。
func openCodeTestEnv(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	restoreExtra := openCodeExtraPaths
	openCodeExtraPaths = func() []string { return nil }
	t.Cleanup(func() { openCodeExtraPaths = restoreExtra })

	bin := t.TempDir()
	script := "#!/bin/sh\necho \"" + version + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return dir
}

// TestOpenCodePluginPath 测试两代插件的路径解析：0.x 用 plugin/，2.x 用
// plugins/；扩展名固定 .ts（两代都不认 .mjs）。
func TestOpenCodePluginPath(t *testing.T) {
	dir := openCodeTestEnv(t, "0.14.3")

	v1, err := openCodePluginPath(openCodeV1)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "opencode", "plugin", "devpilot-hook.ts"); v1 != want {
		t.Fatalf("v1 路径 = %s, 期望 %s", v1, want)
	}

	v2, err := openCodePluginPath(openCodeV2)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "opencode", "plugins", "devpilot-hook.ts"); v2 != want {
		t.Fatalf("v2 路径 = %s, 期望 %s", v2, want)
	}
}

// TestDetectOpenCodeVariant 版本探测：0.x → v1，2.x → v2，找不到 → v2。
func TestDetectOpenCodeVariant(t *testing.T) {
	cases := []struct {
		version  string
		want     openCodeVariant
		found    bool
		wantText string
	}{
		{"0.14.3", openCodeV1, true, "0.14.3"},
		{"opencode v2.2.6", openCodeV2, true, "2.2.6"},
		{"1.19.5", openCodeV1, true, "1.19.5"},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			openCodeTestEnv(t, c.version)
			v, version, found := detectOpenCodeVariant()
			if v != c.want || found != c.found || version != c.wantText {
				t.Fatalf("detect = (%s,%q,%v), 期望 (%s,%q,%v)",
					v, version, found, c.want, c.wantText, c.found)
			}
		})
	}

	// PATH 上没有 opencode：回落 v2，found=false。
	t.Run("missing", func(t *testing.T) {
		restoreExtra := openCodeExtraPaths
		openCodeExtraPaths = func() []string { return nil }
		t.Cleanup(func() { openCodeExtraPaths = restoreExtra })
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())
		v, _, found := detectOpenCodeVariant()
		if found || v != openCodeV2 {
			t.Fatalf("缺 opencode 应回落 v2/false，实际 %s/%v", v, found)
		}
	})
}

// TestInstallOpenCodePluginV1Lifecycle 0.x：写 plugin/、幂等、升级改写、卸载删除。
func TestInstallOpenCodePluginV1Lifecycle(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	exe := "/opt/homebrew/bin/devpilot-hook"

	changed, variant, path, err := installOpenCodePlugin(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || variant != openCodeV1 {
		t.Fatalf("首装应写入 v1，实际 changed=%v variant=%s", changed, variant)
	}
	if want, _ := openCodePluginPath(openCodeV1); path != want {
		t.Fatalf("路径 = %s, 期望 %s", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, openCodePluginMarker) {
		t.Fatal("插件文件缺少身份标记")
	}
	if !strings.Contains(source, "export const DevPilotHook") {
		t.Fatal("v1 插件缺少命名导出 DevPilotHook")
	}
	if !strings.Contains(source, `"permission.ask"`) {
		t.Fatal("v1 插件缺少 permission.ask 钩子")
	}
	if !strings.Contains(source, `["hook", "opencode"]`) {
		t.Fatal("插件应经 devpilot-hook hook opencode 子进程走网关")
	}

	// 幂等：同样内容再装不改。
	changed, _, _, err = installOpenCodePlugin(exe)
	if err != nil || changed {
		t.Fatalf("二次安装应幂等,实际 changed=%v err=%v", changed, err)
	}

	// 升级：EXE 变化后重装改写。
	changed, _, _, err = installOpenCodePlugin("/usr/local/bin/devpilot-hook")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("EXE 变化后应改写")
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "/usr/local/bin/devpilot-hook") {
		t.Fatal("升级后 EXE 未更新")
	}

	// 卸载只删自家文件。
	changed, removed, err := uninstallOpenCodePlugin()
	if err != nil || !changed {
		t.Fatalf("卸载应删除,实际 changed=%v err=%v", changed, err)
	}
	if len(removed) != 1 || removed[0] != path {
		t.Fatalf("removed = %v, 期望 [%s]", removed, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("卸载后插件文件应不存在")
	}
	changed, removed, err = uninstallOpenCodePlugin()
	if err != nil || changed || len(removed) != 0 {
		t.Fatalf("重复卸载应 no-op,实际 changed=%v removed=%v err=%v", changed, removed, err)
	}
}

// TestInstallOpenCodePluginV2 2.x：写 plugins/ 且用 v2 API。
func TestInstallOpenCodePluginV2(t *testing.T) {
	openCodeTestEnv(t, "opencode v2.2.6")
	changed, variant, path, err := installOpenCodePlugin("/opt/homebrew/bin/devpilot-hook")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || variant != openCodeV2 {
		t.Fatalf("应安装 v2，实际 changed=%v variant=%s", changed, variant)
	}
	if want, _ := openCodePluginPath(openCodeV2); path != want {
		t.Fatalf("路径 = %s, 期望 %s", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, "export default DevPilotHook") {
		t.Fatal("v2 插件缺少默认导出")
	}
	if !strings.Contains(source, `id: "devpilot-hook"`) {
		t.Fatal("v2 插件缺少稳定 id")
	}
	if !strings.Contains(source, `permission.hook("evaluate"`) {
		t.Fatal("v2 插件缺少 permission.hook(\"evaluate\") 审批点")
	}
	if !strings.Contains(source, `ctx.tool.hook("execute.before"`) {
		t.Fatal("v2 插件缺少 tool execute.before 钩子")
	}
	if !strings.Contains(source, `ctx.event.subscribe()`) {
		t.Fatal("v2 插件缺少事件订阅")
	}
}

// TestInstallOpenCodePluginSwitchesVariant opencode 升级/降级后重装会把
// 另一代际的自家文件清掉，避免 OpenCode 加载到错误格式。
func TestInstallOpenCodePluginSwitchesVariant(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	if _, _, _, err := installOpenCodePlugin("/bin/devpilot-hook"); err != nil {
		t.Fatal(err)
	}
	v1Path, _ := openCodePluginPath(openCodeV1)
	if _, err := os.Stat(v1Path); err != nil {
		t.Fatal(err)
	}

	// 模拟升级到 2.x：换一个输出新版本的假 opencode。
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\necho \"2.2.6\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	changed, variant, v2Path, err := installOpenCodePlugin("/bin/devpilot-hook")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || variant != openCodeV2 {
		t.Fatalf("升级后应装 v2，实际 changed=%v variant=%s", changed, variant)
	}
	if _, err := os.Stat(v1Path); !os.IsNotExist(err) {
		t.Fatal("v1 插件应被清理")
	}
	if _, err := os.Stat(v2Path); err != nil {
		t.Fatal("v2 插件应存在")
	}
}

// TestInstallOpenCodeRefusesForeignPlugin 用户自己的同名插件绝不被覆盖。
func TestInstallOpenCodeRefusesForeignPlugin(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	path, _ := openCodePluginPath(openCodeV1)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const Mine = async () => ({});\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := installOpenCodePlugin("/opt/homebrew/bin/devpilot-hook"); err == nil {
		t.Fatal("覆盖非自家插件应报错")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "export const Mine") {
		t.Fatal("用户插件内容被改动")
	}
}

// TestUninstallOpenCodeKeepsForeignPlugin 不带标记的文件卸载时原样保留。
func TestUninstallOpenCodeKeepsForeignPlugin(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	path, _ := openCodePluginPath(openCodeV1)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const Mine = async () => ({});\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, removed, err := uninstallOpenCodePlugin()
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(removed) != 0 {
		t.Fatal("非自家插件不应被删")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("用户插件应保留")
	}
}

// TestInstallOpenCodeCleansLegacyMJS 旧实现写在 plugins/*.mjs，安装时清掉。
func TestInstallOpenCodeCleansLegacyMJS(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	legacy, _ := openCodeLegacyPluginPath()
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("// "+openCodePluginMarker+" v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := installOpenCodePlugin("/bin/devpilot-hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("旧 .mjs 应被清理")
	}
}

// TestInspectOpenCodePluginReportsMismatch 插件代际与本机版本不符时
// doctor 要能识别（0.x 装了 v2 文件）。
func TestInspectOpenCodePluginReportsMismatch(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	v2Path, _ := openCodePluginPath(openCodeV2)
	if err := os.MkdirAll(filepath.Dir(v2Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v2Path, []byte("// "+openCodePluginMarker+" v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := inspectOpenCodePlugin()
	if !st.Installed || st.InstalledVariant != openCodeV2 || st.ExpectedVariant != openCodeV1 {
		t.Fatalf("状态 = %+v", st)
	}
	if res := checkOpenCodePlugin(); res.Status != "warn" {
		t.Fatalf("代际不符应 warn，实际 %+v", res)
	}
}

// TestOpenCodePluginV1PostToolUseCarriesArgs 0.x 的 tool.execute.after 事件
// 不带工具实参（output 只有 title/output/metadata），PostToolUse 的
// tool_input 必须取 before 缓存的同一 callID——读 input.args 恒得空对象，
// 遥测里的已执行命令详情会整体丢失。
func TestOpenCodePluginV1PostToolUseCarriesArgs(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	source := openCodePluginSource(openCodeV1, "/bin/devpilot-hook")
	if !strings.Contains(source, "calls.set(input.callID") {
		t.Fatal("v1 before 钩子应缓存工具实参")
	}
	if !strings.Contains(source, "calls.get(input.callID)") {
		t.Fatal("v1 after 钩子应复用 before 缓存的实参")
	}
	if strings.Contains(source, "input && input.args") {
		t.Fatal("after 事件没有 input.args，读它恒得空对象")
	}
}

// TestOpenCodePluginV2SharesDenyFallback v2 插件的 deny 兜底文案由 Go 侧
// 注入 agent.DenyFallbackMessage——插件与三个 CLI 适配器共用一份常量，
// 模板里不允许出现未替换的 token。
func TestOpenCodePluginV2SharesDenyFallback(t *testing.T) {
	openCodeTestEnv(t, "opencode v2.2.6")
	source := openCodePluginSource(openCodeV2, "/bin/devpilot-hook")
	if !strings.Contains(source, agent.DenyFallbackMessage) {
		t.Fatal("v2 插件应注入共享兜底文案")
	}
	if strings.Contains(source, "__DENY_FALLBACK__") {
		t.Fatal("兜底文案 token 未被替换")
	}
}

// TestInstallOpenCodeSucceedsWhenStaleCleanupFails 残留清理失败不算安装
// 失败：目标文件已写入/已最新时 install 仍应成功（此前会把成功安装报成
// "failed to install" 退出 1，serve 自愈每次启动重放同一警告）。残留由
// doctor 的代际/legacy 检查兜底可见。
func TestInstallOpenCodeSucceedsWhenStaleCleanupFails(t *testing.T) {
	openCodeTestEnv(t, "0.14.3")
	legacy, _ := openCodeLegacyPluginPath()
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("// "+openCodePluginMarker+" v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 锁死 legacy 所在目录：目录不可写时 unlink 恒 EACCES（模拟权限锁定）。
	pluginsDir := filepath.Dir(legacy)
	if err := os.Chmod(pluginsDir, 0o555); err != nil {
		t.Fatal(err)
	}
	// 先恢复目录权限再让 TempDir 自清（t.Cleanup 后进先出）。
	t.Cleanup(func() { _ = os.Chmod(pluginsDir, 0o755) })

	// 首装路径：v1 文件写入成功即算成功，残留清理失败不外溢。
	changed, variant, _, err := installOpenCodePlugin("/bin/devpilot-hook")
	if err != nil {
		t.Fatalf("清理失败不应使安装报错: %v", err)
	}
	if !changed || variant != openCodeV1 {
		t.Fatalf("首装应 changed=true/v1，实际 changed=%v variant=%s", changed, variant)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("锁定目录里的残留文件应原样保留（供 doctor 上报）")
	}

	// 幂等路径（内容已最新 + 清理失败）同样成功。
	changed, _, _, err = installOpenCodePlugin("/bin/devpilot-hook")
	if err != nil || changed {
		t.Fatalf("up-to-date + 清理失败应 (false,nil)，实际 (%v,%v)", changed, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("残留文件应仍保留")
	}
}

// TestParseAgentsAcceptsOpenCode 白名单放行 opencode。
func TestParseAgentsAcceptsOpenCode(t *testing.T) {
	agents, err := parseAgents("claude,opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[1] != "opencode" {
		t.Fatalf("agents = %v", agents)
	}
	if _, err := parseAgents("opencode"); err != nil {
		t.Fatalf("单独 opencode 应合法: %v", err)
	}
}

// TestHookCmdAcceptsOpenCodeArg hook 子命令放行 opencode（插件经 CLI 走
// 网关）;未识别的 agent 立即静默退出,绝不输出到 stdout 污染 Agent。
func TestHookCmdAcceptsOpenCodeArg(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	var stdout, stderr strings.Builder
	// 无配置/token 的空环境:应静默 0 退出(绝不破坏 Agent),不写 stdout。
	code := runHook([]string{"opencode"}, strings.NewReader(`{}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if stdout.String() != "" {
		t.Fatalf("失败路径不应输出决策,实际 %q", stdout.String())
	}
}

// TestFindStableExecutableUsesArgv0 模拟 launchd 的窄 PATH：LookPath 失败时
// 用 argv[0] 的稳定路径（不是解析到 Caskroom 的真实路径）。
func TestFindStableExecutableUsesArgv0(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "devpilot-hook")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := os.Args
	os.Args = []string{exe}
	t.Cleanup(func() { os.Args = old })

	got, err := findStableExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("稳定路径 = %s, 期望 argv[0] %s", got, exe)
	}
}
