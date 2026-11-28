// Package gateway 实现 127.0.0.1 上的本地 hook 网关。
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
	"github.com/devpilot-app/hook/internal/event"
	"github.com/devpilot-app/hook/internal/id"
	"github.com/devpilot-app/hook/internal/queue"
)

// MaxBodyBytes 是单个 hook 请求体上限。
const MaxBodyBytes = 1 << 20

// Publisher 把事件交给上行通道；必须快速返回，真正重试由 WAL 保证。
type Publisher interface {
	Publish(ctx context.Context, env wire.Envelope) error
}

// Params 是网关依赖。
type Params struct {
	Addr              string
	Token             string
	DaemonID          string
	ApprovalTimeout   time.Duration
	ToolEventInterval time.Duration
	ApprovalMode      config.ApprovalMode
	// DisableAnswerQuestions 关闭 AskUserQuestion 的问题卡片（默认开）：
	// 开着时 PreToolUse 的 AskUserQuestion 发「只通知不阻塞」的审批事件
	// （NeedsApproval=false，hook 立即放行让原生问题框回终端，手机卡只做
	// 告知 + 导航）；关掉后退化成 tool_running 遥测，不发卡。
	DisableAnswerQuestions bool
	Adapters               map[wire.Agent]agent.Adapter
	Registry          *event.Registry
	// Calls 是 tool_use_id ↔ 审批的关联索引；nil 时 New 自建（测试路径）。
	// serve 把同一实例交给 janitor 做 TTL 清扫。
	Calls             *event.CallIndex
	WAL               *queue.WAL
	Publisher         Publisher
	Now               func() time.Time
	Logger            *slog.Logger
	// HostInfo 返回主机身份快照(serve 维护的 provider,含 TTL 刷新);
	// nil 时跳过盖章(测试/旧路径)。
	HostInfo func() agent.HostInfo
	// ExtraStatus 供 doctor 汇总上行通道状态。
	ExtraStatus func() map[string]any
}

// Server 是本地网关。
type Server struct {
	p        Params
	throttle *event.Throttle
	log      *slog.Logger

	mu   sync.Mutex
	addr string

	// anchors 是会话锚点缓存（session_id → 归属），见 sessionAnchor。
	anchorMu sync.Mutex
	anchors  map[string]sessionAnchor

	// notifyCards 是「只通知不阻塞」等待卡的会话缓存(session_id → 卡)。
	// Gemini/Grok 的 Notification 载荷没有 tool_use_id,审批 ID 是随机的,
	// 事后无法从 PostToolUse 确定性派生同一 id——只能按会话记账:同会话
	// 的下一个工具事件(开跑/结束/回合完成/会话结束)说明终端已处置,
	// 补发 close(cancelled) 收卡。
	notifyMu    sync.Mutex
	notifyCards map[string]wire.Envelope

	// calls 是 tool_use_id ↔ 审批的关联索引（见 event.CallIndex）：Claude
	// Code 的 PermissionRequest 载荷不带 tool_use_id，卡片收场靠它把
	// PreToolUse / PermissionRequest / PostToolUse 三路事件对上号。只存内存，
	// daemon 重启即失（退化为卡等超时，方向安全）。
	calls *event.CallIndex
}

// sessionAnchor 是一条 agent 会话的稳定归属：项目根 + tmux 坐标。
//
// hook 对每个事件都现探 tmux，但探测会失败（引擎后台化后进程树脱离
// tmux、ps/tmux 偶发失败）——失败一次，那张卡的 Answer at Terminal 就没有
// 坐标，App 只能新开同名会话跑 resume fork、进错会话。而 agent 进程在
// 会话期间不搬家：它的 tmux 窗格是会话级常量。会话内探到的坐标缓存
// 起来，后续事件探空时兜底。
type sessionAnchor struct {
	projectDir string
	tmux       tmuxAnchor
	seen       time.Time
}

// tmuxAnchor 是 tmux 定位坐标（会话名 / 窗口号 / 窗格 id）。
type tmuxAnchor struct{ session, window, pane string }

const (
	// anchorTTL 是会话锚点的保留期：覆盖最长的审批等待窗（6h）再留余量。
	anchorTTL = 12 * time.Hour
	// anchorSweepThreshold 是触发过期清扫的缓存条数（按会话数计，很小）。
	anchorSweepThreshold = 256
)

// New 校验依赖并创建网关。
func New(p Params) (*Server, error) {
	if p.Token == "" {
		return nil, errors.New("gateway: missing local token")
	}
	if p.DaemonID == "" {
		return nil, errors.New("gateway: missing daemon_id")
	}
	if p.Registry == nil || p.WAL == nil {
		return nil, errors.New("gateway: missing Registry or WAL")
	}
	if p.Calls == nil {
		p.Calls = event.NewCallIndex(0)
	}
	if len(p.Adapters) == 0 {
		return nil, errors.New("gateway: no agent adapters registered")
	}
	if p.ApprovalTimeout <= 0 {
		p.ApprovalTimeout = wire.ApprovalWaitSeconds * time.Second
	}
	if p.ApprovalMode == "" {
		p.ApprovalMode = config.ApprovalModePermissionRequest
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return &Server{
		p:          p,
		throttle:   event.NewThrottle(p.ToolEventInterval),
		log:        p.Logger,
		addr:       p.Addr,
		anchors:    make(map[string]sessionAnchor),
		notifyCards: make(map[string]wire.Envelope),
		calls:      p.Calls,
	}, nil
}

// SweepCalls 对关联索引做一次 TTL/失联清扫（serve 的 janitor 与 Registry
// Cleanup 同车次调用）。TTL 取审批超时 +10min：正常生命周期由 PostToolUse
// Forget / SessionEnd ForgetSession 收口，TTL 只兜进程异常路径的泄漏。
func (s *Server) SweepCalls(now time.Time, ttl time.Duration) int {
	return s.calls.Sweep(now, ttl, func(id string) bool { return s.p.Registry.IsPending(id) })
}

// Addr 返回实际监听地址（监听前返回配置值）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Handler 返回 HTTP 处理器（测试可直接挂到 httptest）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.localOnly(s.handleHealth))
	mux.HandleFunc("GET /v1/status", s.guard(s.handleStatus))
	mux.HandleFunc("POST /v1/hook/{agent}", s.guard(s.handleHook))
	mux.HandleFunc("POST /v1/events", s.guard(s.handleEvent))
	mux.HandleFunc("POST /v1/decisions", s.guard(s.handleDecision))
	return mux
}

// ListenAndServe 阻塞运行，直到 ctx 取消。
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.p.Addr)
	if err != nil {
		return fmt.Errorf("gateway: listen failed: %w", err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	s.log.Info("local gateway started", "addr", s.addr)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localOnly 只允许本机回环访问，并拒绝浏览器发起的跨站请求。
func (s *Server) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "browser-origin requests are not allowed")
			return
		}
		if !validHost(r.Host) {
			writeError(w, http.StatusForbidden, "host must be a loopback address")
			return
		}
		next(w, r)
	}
}

// guard 在 localOnly 之上校验 Bearer token。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return s.localOnly(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(s.p.Token) ||
			subtle.ConstantTimeCompare([]byte(got), []byte(s.p.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid local token")
			return
		}
		next(w, r)
	})
}

func validHost(host string) bool {
	h := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		h = name
	}
	switch h {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"daemon_id":         s.p.DaemonID,
		"gateway_addr":      s.Addr(),
		"pending_approvals": s.p.Registry.Len(),
		"queue_depth":       s.p.WAL.Len(),
	}
	if s.p.ExtraStatus != nil {
		maps.Copy(resp, s.p.ExtraStatus())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := wire.Agent(r.PathValue("agent"))
	adapter, ok := s.p.Adapters[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unsupported agent: "+string(name))
		return
	}

	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	in, err := agent.ParseInputAs(string(name), body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.p.Now()
	// 会话归属先于事件构造：标题取项目根，不能等 payload 生成完再补。
	anchor := s.resolveAnchor(r, in.SessionID, now)
	in.ProjectDir = anchor.projectDir

	// 选项作答默认开（主路径特性），配置侧 disable_answer_questions 显式关闭。
	ev, err := adapter.Normalize(in, s.p.DaemonID, now.Unix(), s.p.ApprovalMode, !s.p.DisableAnswerQuestions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	env := ev.Envelope
	s.stampHostContext(&env, in, anchor)

	// 终端已答对账（2026-11-28 真机滞卡案的收场主路径）。Claude Code 2.1.332
	// 里用户答完原生弹窗后 hook 进程并不退出——阻塞中的 PermissionRequest
	// 等不到任何信号；「已答完」的证据只能来自后续事件：
	//   - PostToolUse：工具真的跑完了 = 终端已批，按 tool_use_id 关联收卡；
	//   - Stop / SessionEnd：回合/会话收尾时仍 pending 的审批 = 终端已拒或
	//     被打断（无事件可依赖，只能对账兜底）。
	// 必须先于节流：收场是审批语义事件，不受 300s 遥测窗口约束。也必须先于
	// 本请求的 enqueue：session_ended 前先逐卡 close，relay 端 token 随收场
	// 剪除，会话收尾循环就不再重复 end。
	switch in.HookEventName {
	case "PreToolUse":
		if in.ToolUseID != "" {
			s.calls.Observe(event.Call{
				SessionID:   in.SessionID,
				ToolUseID:   in.ToolUseID,
				ToolName:    in.ToolName,
				InputDigest: wire.InputDigest(in.ToolInput),
			})
		}
	case "PostToolUse":
		if in.ToolUseID != "" {
			s.reconcilePostToolUse(in)
		}
	case "Stop":
		s.sweepSessionApprovals(env.SessionID, now.Add(-stopSweepGrace), "stop_sweep")
	case "SessionEnd":
		s.sweepSessionApprovals(env.SessionID, now, "session_end_sweep")
		s.calls.ForgetSession(env.SessionID)
	}

	// 高频工具遥测做固定窗口节流；审批、任务完成、会话边界永不丢弃。
	if env.Type == wire.EventToolRunning || env.Type == wire.EventToolFinished {
		if !s.throttle.Allow(env.SessionID+":"+string(env.Type), now) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if env.Type == wire.EventSessionEnded {
		s.throttle.Forget(env.SessionID + ":")
		s.forgetAnchor(env.SessionID)
	}

	// 「只通知不阻塞」等待卡的生命周期:同会话出现工具开跑/结束、回合
	// 完成、会话结束 = 终端已处置(批准/拒绝/换工具),补发 close(cancelled)
	// 收卡(卡没有 tool_use_id,无法从 PostToolUse 确定性收,只能按会话
	// 记账,见 notifyCards)。close 走独立入队,不受本请求 204 影响。
	switch env.Type {
	case wire.EventToolRunning, wire.EventToolFinished, wire.EventTaskComplete, wire.EventSessionEnded:
		if old := s.forgetNotifyCard(env.SessionID); old != nil {
			s.emitClose(*old, event.StatusCancelled)
		}
	}

	if !ev.NeedsApproval {
		// 等待卡登记:同会话已有旧卡时补发 close(被新卡取代)。
		if env.Type == wire.EventApprovalRequired && env.Payload.NotifyOnly {
			if old := s.rememberNotifyCard(env); old != nil {
				s.emitClose(*old, event.StatusCancelled)
			}
		}
		// 问题卡/等待卡同样登记 Registry（不 Wait）：PostToolUse 按
		// tool_use_id 关联收卡、Stop/SessionEnd sweep 兜底，与 notifyCards
		// 会话记账并存——两条路径的 close 同为 "close-<id>"，relay 入箱去重
		// 收敛，不会双推。
		if env.Type == wire.EventApprovalRequired {
			s.p.Registry.Add(env, now, s.p.ApprovalTimeout)
			if in.ToolUseID != "" {
				s.calls.Link(in.SessionID, in.ToolUseID, env.ApprovalID)
			}
		}
		if err := s.enqueue(r.Context(), env); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 审批先登记再入队：重复 hook 不会产生第二条审批。
	_, isNew := s.p.Registry.Add(env, now, s.p.ApprovalTimeout)
	if isNew {
		if err := s.enqueue(r.Context(), env); err != nil {
			s.p.Registry.Cancel(env.ApprovalID)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// PermissionRequest 载荷缺 tool_use_id，但 PreToolUse（几十毫秒前刚过）
	// 带着：按 digest 反查关联，把本次审批挂到那次调用上——PostToolUse 才
	// 能沿 use_id 找到这张卡。digest 落空退同会话同名调用最新未关联条目
	// （仅关联时刻允许按名兜底，收场路径严禁）。
	if in.ToolUseID == "" {
		if useID, ok := s.calls.ResolveUse(in.SessionID, in.ToolName, env.Payload.InputDigest); ok {
			s.calls.Link(in.SessionID, useID, env.ApprovalID)
		}
	} else {
		s.calls.Link(in.SessionID, in.ToolUseID, env.ApprovalID)
	}

	result := s.p.Registry.Wait(r.Context(), env.ApprovalID)
	if result.External {
		// 终端里已答完：close 已由对账方发出，此处不得重复发；原生弹窗也已
		// 答完，渲染任何决策都是错的——空 204 结束。
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch result.Status {
	case event.StatusApproved, event.StatusDenied:
		// approved/denied 不发 close:决策经由 relay,relay 落地时已合成广播。
		out, err := adapter.RenderDecision(ev.HookEvent, result.Decision, result.Choice, in.ToolName, in.ToolInput)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Gemini/Grok 的批准输出是「空 stdout」(exit 0 即放行):nil 不落体,
		// 但仍 200——客户端拿到 200 才会原样放行。
		if out == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(out, '\n'))
	default:
		// 闸门审批(Gemini yolo / Grok always-approve)没有原生弹窗可回落:
		// 超时/取消必须 fail-closed——200 + deny 让工具被拦,而不是 204 放行。
		// 手机侧照常收场(emitClose,expired → 「Missed」提醒是准确的)。
		if ev.Gate {
			out, err := adapter.RenderDecision(ev.HookEvent, wire.DecisionDeny, "", in.ToolName, in.ToolInput)
			if err == nil && out != nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(append(out, '\n'))
			} else {
				w.WriteHeader(http.StatusOK)
			}
			s.emitClose(env, result.Status)
			return
		}
		// 超时 / 取消：不输出任何 decision，让 Agent 回落原生询问；
		// 但手机侧横幅与 LA 需要收场——发 close(与 relay 决策/清理两条
		// 合成路径共用确定性 event_id,inbox 去重自然收敛)。
		s.emitClose(env, result.Status)
		w.WriteHeader(http.StatusNoContent)
	}
}

// stampHostContext 把主机身份与会话上下文盖进事件 payload:
// hostname/host_ip/user/cwd 供 App 收件箱路由(匹配 HostRecord → 直进会话);
// project_dir 与 tmux 坐标来自会话锚点(本次探测命中，或会话缓存兜底);
// approval_required 额外盖 expires_at——等待窗终点,relay pending TTL、
// APNs 过期、LA 倒计时全链对齐这一处时间源。
func (s *Server) stampHostContext(env *wire.Envelope, in agent.Input, anchor sessionAnchor) {
	if s.p.HostInfo != nil {
		h := s.p.HostInfo()
		env.Payload.Hostname = h.Hostname
		env.Payload.HostIP = h.PrimaryIP
		env.Payload.User = h.User
	}
	env.Payload.Cwd = in.Cwd
	if anchor.projectDir != "" {
		env.Payload.ProjectDir = anchor.projectDir
	}
	// tmux 窗口号(v0.3.1)与窗格 id:与 session 同源探测,App 详情抽屉展示
	// / 直达时定位;仅探测名单事件有 header,工具遥测事件天然不带。
	if anchor.tmux.session != "" {
		env.Payload.TmuxSession = anchor.tmux.session
		env.Payload.TmuxWindow = anchor.tmux.window
		env.Payload.TmuxPane = anchor.tmux.pane
	}
	if env.Type == wire.EventApprovalRequired {
		env.ExpiresAt = s.p.Now().Unix() + int64(s.p.ApprovalTimeout/time.Second)
	}
}

// resolveAnchor 合并本次事件的 header 与会话缓存，返回该会话的归属：
//   - header 带了 tmux 坐标 / 项目根 → 以它为准并刷新缓存（探到了就是
//     最新真值）；
//   - header 没带而缓存仍在保留期内 → 用缓存兜底；
//   - 两者皆空（会话真的不在 tmux 附近）→ 空锚点，宁缺毋滥。
//
// 旧版 hook（不带 X-DevPilot-Project-Dir / X-DevPilot-Tmux-Pane）照常工作。
func (s *Server) resolveAnchor(r *http.Request, sessionID string, now time.Time) sessionAnchor {
	if sessionID == "" {
		return sessionAnchor{projectDir: strings.TrimSpace(r.Header.Get("X-DevPilot-Project-Dir"))}
	}
	tmux := tmuxAnchor{
		session: strings.TrimSpace(r.Header.Get("X-DevPilot-Tmux-Session")),
		window:  strings.TrimSpace(r.Header.Get("X-DevPilot-Tmux-Window")),
		pane:    strings.TrimSpace(r.Header.Get("X-DevPilot-Tmux-Pane")),
	}
	if tmux.session == "" {
		// 没有会话名时窗口号/窗格 id 无处可挂，不单独带出。
		tmux = tmuxAnchor{}
	}
	anchor := sessionAnchor{
		projectDir: strings.TrimSpace(r.Header.Get("X-DevPilot-Project-Dir")),
		tmux:       tmux,
		seen:       now,
	}

	s.anchorMu.Lock()
	defer s.anchorMu.Unlock()
	cached, ok := s.anchors[sessionID]
	if !ok || now.Sub(cached.seen) > anchorTTL {
		cached = sessionAnchor{}
	}
	if anchor.projectDir == "" {
		anchor.projectDir = cached.projectDir
	}
	if anchor.tmux.session == "" {
		anchor.tmux = cached.tmux
	}
	if anchor.projectDir == "" && anchor.tmux.session == "" {
		delete(s.anchors, sessionID)
		return anchor
	}
	s.anchors[sessionID] = anchor
	s.pruneAnchorsLocked(now)
	return anchor
}

// forgetAnchor 在会话结束后清掉锚点（会话 id 不复用，留着只会占位）。
func (s *Server) forgetAnchor(sessionID string) {
	s.anchorMu.Lock()
	delete(s.anchors, sessionID)
	s.anchorMu.Unlock()
}

// pruneAnchorsLocked 清掉过期锚点；条数不多时跳过（调用方持锁）。
func (s *Server) pruneAnchorsLocked(now time.Time) {
	if len(s.anchors) < anchorSweepThreshold {
		return
	}
	for id, a := range s.anchors {
		if now.Sub(a.seen) > anchorTTL {
			delete(s.anchors, id)
		}
	}
}

// closeReason 把本端 Wait 结果映射为 wire 关闭原因。
func closeReason(st event.Status) string {
	if st == event.StatusExpired {
		return wire.CloseReasonExpired
	}
	// cancelled(终端 Ctrl-C / hook 退出)与其余未知终态一律按取消处理。
	return wire.CloseReasonCancelled
}

// rememberNotifyCard 登记等待卡;同会话已有旧卡时先摘除返回(调用方
// 补发 close——被新卡取代 = 终端已处置)。
func (s *Server) rememberNotifyCard(env wire.Envelope) *wire.Envelope {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notifyCards == nil {
		s.notifyCards = map[string]wire.Envelope{}
	}
	old, ok := s.notifyCards[env.SessionID]
	s.notifyCards[env.SessionID] = env
	if ok && old.ApprovalID != env.ApprovalID {
		return &old
	}
	return nil
}

// forgetNotifyCard 若该会话有等待卡则摘除并返回(调用方补发 close)。
func (s *Server) forgetNotifyCard(sessionID string) *wire.Envelope {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	old, ok := s.notifyCards[sessionID]
	if !ok {
		return nil
	}
	delete(s.notifyCards, sessionID)
	return &old
}

// emitClose 合成 approval_closed 事件并入队(WAL 先行,断网也有补发)。
// 用独立 context:取消场景里 hook 客户端已断开、r.Context() 已取消,
// 上行仍须尝试;失败由 WAL 兜底,relay 侧 event_id 去重防重放重复。
func (s *Server) emitClose(env wire.Envelope, status event.Status) {
	closeEnv := env
	closeEnv.MsgID = id.New()
	closeEnv.EventID = "close-" + env.ApprovalID
	closeEnv.Type = wire.EventApprovalClosed
	closeEnv.ExpiresAt = 0
	closeEnv.TS = s.p.Now().Unix()
	closeEnv.Payload.StatusText = closeReason(status)
	if err := s.enqueue(context.Background(), closeEnv); err != nil {
		s.log.Warn("failed to enqueue approval_closed", "approval_id", env.ApprovalID, "err", err)
	}
}

// stopSweepGrace 是 Stop 对账的宽限窗：PostToolUse 严格先于 Stop（回合收尾
// 要求全部工具调用已了结），宽限只防同秒时钟毛刺，不是并行批次的保护——
// 那由收场键的精确匹配保证。
const stopSweepGrace = 3 * time.Second

// reconcilePostToolUse 处理「工具已跑完 = 终端已批」的收卡：先按 tool_use_id
// 走关联索引（主路径，Pre/Post 输入漂移免疫），落空再按
// (session, tool, input_digest) 精确匹配 pending 审批（无 PreToolUse 登记的
// 老版本兜底）。绝不清链失败就按 tool 名兜底——并行批次会误收仍在弹窗的
// 另一条。收场后照常落 tool_finished 遥测（调用方继续）。
func (s *Server) reconcilePostToolUse(in agent.Input) {
	approvalID := ""
	if id, ok := s.calls.ApprovalFor(in.SessionID, in.ToolUseID); ok {
		approvalID = id
	} else if id, ok := s.p.Registry.PendingMatching(in.SessionID, in.ToolName, wire.InputDigest(in.ToolInput)); ok {
		approvalID = id
	}
	s.calls.Forget(in.SessionID, in.ToolUseID)
	if approvalID == "" {
		return
	}
	s.resolveExternally(approvalID, "post_tool_use")
}

// sweepSessionApprovals 收掉某会话所有到龄仍 pending 的审批（Stop / SessionEnd
// 对账：终端 deny / Esc 不产生任何 hook 事件，这是它们唯一的收场信号）。
func (s *Server) sweepSessionApprovals(sessionID string, oldest time.Time, reason string) {
	for _, apvID := range s.p.Registry.PendingInSession(sessionID, oldest) {
		s.resolveExternally(apvID, reason)
	}
}

// resolveExternally 外部收场单入口：CancelExternal（first-wins，已决/已超时
// 不动）→ 用存量 envelope 发 close(cancelled)——存量包封保留 title / tmux /
// hostname 线索（I8），App 端据此渲染 Answered at terminal 并收卡。
func (s *Server) resolveExternally(approvalID, reason string) {
	env, ok := s.p.Registry.CancelExternal(approvalID)
	if !ok {
		// 手机已决策 / 已超时 / 已被别的对账路径收场：close 归属原路径。
		return
	}
	s.emitClose(env, event.StatusCancelled)
	s.log.Info("approval resolved elsewhere", "approval_id", approvalID, "reason", reason)
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	msg, err := wire.Decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	env, ok := msg.(*wire.Envelope)
	if !ok {
		writeError(w, http.StatusBadRequest, "only kind=event is accepted")
		return
	}
	if env.DaemonID != s.p.DaemonID {
		writeError(w, http.StatusBadRequest, "daemon_id mismatch")
		return
	}
	if err := s.enqueue(r.Context(), *env); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var d wire.Decision
	if err := json.Unmarshal(body, &d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision JSON")
		return
	}
	if err := d.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res := s.p.Registry.Decide(d.ApprovalID, d.Decision, d.Choice, s.p.Now())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   res.Status,
		"decision": res.Decision,
		"accepted": res.Accepted,
	})
}

// enqueue 先落 WAL 再上行；上行失败不影响 hook 正确性，重连后补发。
func (s *Server) enqueue(ctx context.Context, env wire.Envelope) error {
	if err := s.p.WAL.Put(env); err != nil {
		return err
	}
	if s.p.Publisher != nil {
		if err := s.p.Publisher.Publish(ctx, env); err != nil {
			s.log.Debug("event buffered in WAL, will resend after reconnect", "event_id", env.EventID, "err", err)
		}
	}
	return nil
}

func readLimited(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	reader := http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	data, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, maxErr
		}
		return nil, err
	}
	return data, nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
