package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AssistantCapabilities is truthful, bounded metadata describing the
// capabilities composed for one Application. It is descriptive only; the
// executor remains authoritative.
type AssistantCapabilities struct {
	ScopeRoot     string              `json:"scope_root,omitempty"`
	ScopeRoots    []string            `json:"scope_roots,omitempty"`
	WriteRoot     string              `json:"write_root,omitempty"`
	WriteRoots    []string            `json:"write_roots,omitempty"`
	FullMachine   bool                `json:"full_machine,omitempty"`
	Policy        AssistantPolicyMode `json:"policy"`
	WebEnabled    bool                `json:"web_enabled,omitempty"`
	RemoteGranted bool                `json:"remote_granted,omitempty"`
	ToolNames     []string            `json:"tool_names,omitempty"`
	SkillNames    []string            `json:"skill_names,omitempty"`
	PluginNames   []string            `json:"plugin_names,omitempty"`
	// LaunchRoot is the directory Bee was started from when it was made an
	// extra write root (see AssistantCompositionOptions.LaunchRoot).
	LaunchRoot string `json:"launch_root,omitempty"`
}

// AssistantCompositionOptions customizes the unified general-assistant root.
// A nil Store uses the default durable session store. Without an explicit
// scope, an initialized Bee workspace is the primary root and the user's home
// remains a secondary allowed root; FullMachine explicitly opts into the host
// root. No project/repository root is required.
type AssistantCompositionOptions struct {
	Store                 *SessionStore
	JobStore              *JobStore
	JobStorePath          string
	NotificationStore     *NotificationInboxStore
	NotificationStorePath string
	TodoStore             *TodoStore
	TodoStorePath         string
	ScopeRoot             string
	ScopeRoots            []string
	WriteRoot             string
	WriteRoots            []string
	// LaunchRoot is the directory an interactive Bee was started from. When
	// it is safe (not /, the home directory, an ancestor of Bee's home, or
	// inside Bee's state, secrets, or sessions) it becomes an extra readable
	// and writable root, the way coding agents treat the directory they run
	// in. The Bee workspace stays the preferred root for relative paths.
	LaunchRoot           string
	FullMachine          bool
	MemoryRoot           string
	ArtifactStore        *ArtifactStore
	Policy               AssistantPolicyMode
	ApprovalMode         ApprovalMode
	Approval             ApprovalHook
	Auditor              Auditor
	RemoteGrant          *RemoteCapabilityGrant
	CodingWorkspace      WorkspaceManager
	CodingProjectRoot    string
	CodingManagedRoot    string
	CodingBaseCommit     string
	CodingTrace          TraceSink
	CodingLocalExecutor  ChildExecutor
	CodingParentID       string
	OperationsDelegation *OperationsDelegationService
	TerminalManager      *TerminalManager
	TerminalAuditHook    TerminalAuditHook
	MutationJournal      *MutationJournal
	DiagnosticSink       DiagnosticSink
	// DisableDefaultDiagnostics is intended for embedders that supply their own
	// observability lifecycle. Ordinary Bee products write bounded JSONL runtime
	// diagnostics below BEE_HOME/state/diagnostics.
	DisableDefaultDiagnostics bool
	// SkillRegistry injects a versioned skill registry. A nil value opens the
	// default registry only when Bee has already initialized it.
	SkillRegistry *SkillRegistry
	// PluginRegistry injects the versioned external-process plugin registry.
	// A nil value opens the default registry only when it exists.
	PluginRegistry *PluginRegistry
	// ScratchpadStore is private, durable per-session scratch state. A nil value
	// derives a store below the injected SessionStore root.
	ScratchpadStore *ScratchpadStore
	// SystemInspector is an optional Linux-only read-only host inspector. A nil
	// value uses the safe default on supported platforms; locked mode never
	// exposes these tools because it has no executor.
	SystemInspector SystemInspector
	// EnableMediaSearch is a web-gateway-only opt-in. Compatibility adapters
	// such as Telegram leave it false even when the shared config enables the
	// capability.
	EnableMediaSearch bool
}

// ComposeGeneralAssistant builds the one ordinary Bee assistant. The same
// composition is used for chat, inspection, memory lookup, web research, and
// coding tasks; there is no alternate coding product mode.
func ComposeGeneralAssistant(config Config, profile string, options AssistantCompositionOptions) (*Application, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if options.Policy == "" {
		options.Policy = config.Assistant.Policy
		if options.Policy == "" {
			options.Policy = AssistantPolicyTrusted
		}
	}
	if options.Policy != AssistantPolicyTrusted && options.Policy != AssistantPolicyPrompted && options.Policy != AssistantPolicyLocked {
		return nil, fmt.Errorf("assistant policy %q is invalid", options.Policy)
	}
	if options.ApprovalMode == "" {
		options.ApprovalMode = config.Assistant.ApprovalMode.effective()
	}
	if err := options.ApprovalMode.Validate(); err != nil {
		return nil, fmt.Errorf("assistant %w", err)
	}
	if options.FullMachine && (strings.TrimSpace(options.ScopeRoot) != "" || len(options.ScopeRoots) > 0) {
		return nil, errors.New("full-machine scope cannot be combined with scope roots")
	}
	if options.Store == nil {
		var err error
		options.Store, err = NewDefaultSessionStore()
		if err != nil {
			return nil, err
		}
	}
	app, err := NewApplicationForProfile(config, profile, options.Store)
	if err != nil {
		return nil, err
	}
	var enabledSkills []SkillPackage
	var enabledPlugins []PluginPackage
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild {
		seenExtensions := make(map[string]struct{}, len(app.Config.Extensions))
		for _, extension := range app.Config.Extensions {
			seenExtensions[extension.Name] = struct{}{}
		}
		app.packageExtensionTools = make(map[string][]string)
		skillRegistry := options.SkillRegistry
		if skillRegistry == nil {
			var present bool
			skillRegistry, present, err = OpenDefaultSkillRegistry()
			if err != nil {
				return nil, fmt.Errorf("assistant skills: %w", err)
			}
			if !present {
				skillRegistry = nil
			}
		}
		if skillRegistry != nil {
			enabledSkills, err = skillRegistry.Enabled(context.Background())
			if err != nil {
				return nil, fmt.Errorf("assistant skills: %w", err)
			}
			extensions, extensionErr := skillRegistry.EnabledExtensions(context.Background())
			if extensionErr != nil {
				return nil, fmt.Errorf("assistant skill extensions: %w", extensionErr)
			}
			for _, extension := range extensions {
				if _, exists := seenExtensions[extension.Config.Name]; exists {
					return nil, fmt.Errorf("skill extension %q conflicts with a configured extension", extension.Config.Name)
				}
				seenExtensions[extension.Config.Name] = struct{}{}
				app.Config.Extensions = append(app.Config.Extensions, extension.Config)
				app.packageExtensionTools[extension.Config.Name] = append([]string(nil), extension.ExpectedTools...)
			}
			app.SkillRegistry = skillRegistry
		}
		pluginRegistry := options.PluginRegistry
		if pluginRegistry == nil {
			var present bool
			pluginRegistry, present, err = OpenDefaultPluginRegistry()
			if err != nil {
				return nil, fmt.Errorf("assistant plugins: %w", err)
			}
			if !present {
				pluginRegistry = nil
			}
		}
		if pluginRegistry != nil {
			enabledPlugins, err = pluginRegistry.Enabled(context.Background())
			if err != nil {
				return nil, fmt.Errorf("assistant plugins: %w", err)
			}
			extensions, extensionErr := pluginRegistry.EnabledExtensions(context.Background())
			if extensionErr != nil {
				return nil, fmt.Errorf("assistant plugin extensions: %w", extensionErr)
			}
			for _, extension := range extensions {
				if _, exists := seenExtensions[extension.Config.Name]; exists {
					return nil, fmt.Errorf("plugin extension %q conflicts with another configured extension", extension.Config.Name)
				}
				seenExtensions[extension.Config.Name] = struct{}{}
				app.Config.Extensions = append(app.Config.Extensions, extension.Config)
				app.packageExtensionTools[extension.Config.Name] = append([]string(nil), extension.ExpectedTools...)
			}
			app.PluginRegistry = pluginRegistry
		}
	}
	jobStore := options.JobStore
	jobStorePath := ""
	if jobStore == nil {
		jobStorePath = strings.TrimSpace(options.JobStorePath)
		if jobStorePath == "" {
			jobStorePath = strings.TrimSpace(config.Assistant.JobStorePath)
		}
		if jobStorePath == "" {
			jobStorePath, err = DefaultBeeJobStorePath()
			if err != nil {
				return nil, err
			}
		}
		jobStore, err = NewJobStore(jobStorePath)
		if err != nil {
			return nil, fmt.Errorf("assistant jobs: %w", err)
		}
	}
	app.JobStore = jobStore
	if options.NotificationStore != nil {
		app.NotificationStore = options.NotificationStore
	} else if strings.TrimSpace(options.NotificationStorePath) != "" {
		notificationStore, notificationErr := NewNotificationInboxStore(options.NotificationStorePath)
		if notificationErr != nil {
			return nil, fmt.Errorf("assistant notifications: %w", notificationErr)
		}
		app.NotificationStore = notificationStore
	} else {
		if jobStorePath == "" {
			jobStorePath = jobStore.Path()
		}
		notificationStore, notificationErr := NewNotificationInboxStore(NotificationStorePathForJobStore(jobStorePath))
		if notificationErr != nil {
			return nil, fmt.Errorf("assistant notifications: %w", notificationErr)
		}
		app.NotificationStore = notificationStore
	}
	var todoStore *TodoStore
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild {
		todoStore = options.TodoStore
		if todoStore == nil {
			todoPath := strings.TrimSpace(options.TodoStorePath)
			if todoPath == "" {
				todoPath = strings.TrimSpace(config.Assistant.TodoStorePath)
			}
			if todoPath == "" {
				todoPath, err = DefaultBeeTodoStorePath()
				if err != nil {
					return nil, err
				}
			}
			todoStore, err = NewTodoStore(todoPath)
			if err != nil {
				return nil, fmt.Errorf("assistant todos: %w", err)
			}
		}
		app.TodoStore = todoStore
	}

	roots, fullMachine, err := resolveAssistantScopes(config.Assistant, options.ScopeRoot, options.ScopeRoots, options.FullMachine)
	if err != nil {
		return nil, err
	}
	writeRoots, err := resolveAssistantWriteScopes(config.Assistant, options.WriteRoot, options.WriteRoots, roots, fullMachine)
	if err != nil {
		return nil, err
	}
	launchRoot := ""
	if candidate := strings.TrimSpace(options.LaunchRoot); candidate != "" && !fullMachine {
		if dir, reason := EligibleLaunchRoot(candidate, writeRoots); reason == "" {
			launchRoot = dir
			if !pathWithinAny(roots, dir) {
				roots = append(roots, dir)
			}
			writeRoots = append(writeRoots, dir)
		}
	}
	root := roots[0]
	scope := ExecutionScope{Root: root, Roots: append([]string(nil), roots[1:]...), WriteRoot: writeRoots[0], WriteRoots: append([]string(nil), writeRoots[1:]...), EnforceWriteRoots: !config.Assistant.UnsafeDisableProcessSandbox, TrustedForeground: options.Policy != AssistantPolicyLocked, RemoteGrant: options.RemoteGrant, LaunchRoot: launchRoot}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("assistant scope: %w", err)
	}
	if scope.EnforceWriteRoots {
		if state, stateErr := DefaultBeeStateRoot(); stateErr == nil {
			scope.SandboxTmpRoot = filepath.Join(state, "runtime", "sandbox-tmp")
			PruneSandboxTmp(scope.SandboxTmpRoot, MaxSandboxTmpAge, time.Now())
		}
	}

	memoryRoot := strings.TrimSpace(options.MemoryRoot)
	if memoryRoot == "" {
		memoryRoot = strings.TrimSpace(config.Assistant.MemoryRoot)
	}
	if memoryRoot == "" {
		memoryRoot, err = DefaultBeeMemoryRoot()
		if err != nil {
			return nil, err
		}
	}
	memoryPolicy := DefaultMemoryPolicy()
	memoryService, err := NewLocalMemoryService(memoryRoot, memoryPolicy)
	if err != nil {
		return nil, fmt.Errorf("assistant memory: %w", err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"}
	memoryRetrieval := config.Semantic.RetrievalOptions(memoryRoot)
	if config.Semantic.Enabled {
		indexRoot, indexErr := config.Semantic.EffectiveIndexRoot()
		embedder, embedderErr := config.Semantic.NewEmbedder()
		if indexErr == nil && embedderErr == nil {
			refresher, refreshErr := NewSemanticIndexRefresher(memoryRoot, indexRoot, config.Semantic.EffectiveModel(), embedder)
			if refreshErr == nil {
				memoryService.SetMutationHook(refresher.Notify)
				app.SemanticRefresher = refresher
			}
		}
	}
	artifactStore := options.ArtifactStore
	if artifactStore == nil {
		artifactStore, err = NewDefaultArtifactStore(options.Store)
		if err != nil {
			return nil, fmt.Errorf("assistant artifacts: %w", err)
		}
	} else if artifactStore.store == nil || filepath.Clean(artifactStore.store.Root()) != filepath.Clean(options.Store.Root()) {
		return nil, errors.New("assistant artifact store must use the configured session store")
	}
	transcriptionScript, err := config.Speech.resolveTranscriptionScript()
	if err != nil {
		return nil, fmt.Errorf("assistant speech: %w", err)
	}
	attachmentStore, err := NewAttachmentStore(filepath.Join(artifactStore.Root(), "attachments"), artifactStore, AttachmentStoreOptions{AudioScriptPath: transcriptionScript})
	if err != nil {
		return nil, fmt.Errorf("assistant attachments: %w", err)
	}
	scratchpadStore := options.ScratchpadStore
	if scratchpadStore == nil {
		scratchpadStore, err = NewDefaultScratchpadStore(options.Store)
		if err != nil {
			return nil, fmt.Errorf("assistant scratchpad: %w", err)
		}
	}
	systemInspector := options.SystemInspector
	systemInspectorProvided := systemInspector != nil
	if systemInspector == nil && runtime.GOOS == "linux" {
		systemInspector, err = NewLinuxSystemInspector()
		if err != nil {
			return nil, fmt.Errorf("assistant system inspector: %w", err)
		}
	}
	mutationJournal := options.MutationJournal
	if mutationJournal == nil {
		journalBase := options.Store.Root()
		if strings.TrimSpace(journalBase) == "" {
			return nil, errors.New("assistant mutation journal requires a session store root")
		}
		journalScopeRoots, stateErr := scope.rootPaths()
		if stateErr != nil {
			return nil, fmt.Errorf("assistant mutation journal roots: %w", stateErr)
		}
		mutationJournal, stateErr = NewMutationJournalForRoots(filepath.Join(journalBase, "mutations"), journalScopeRoots)
		if stateErr != nil {
			return nil, fmt.Errorf("assistant mutation journal: %w", stateErr)
		}
	}
	registry, err := newGeneralAssistantRegistry(scope, memoryService, actor, memoryPolicy, memoryRetrieval, jobStore, artifactStore, todoStore, mutationJournal, app.MemoryReranker)
	if err != nil {
		return nil, err
	}
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild && len(config.LSP.Servers) > 0 {
		manager, lspErr := NewLSPManager(config.LSP, scope)
		if lspErr != nil {
			return nil, fmt.Errorf("configure language servers: %w", lspErr)
		}
		if len(manager.hosts) > 0 {
			for _, kind := range []string{"hover", "definition", "references"} {
				if err := registry.Register(LSPQueryTool{Manager: manager, Kind: kind}); err != nil {
					_ = manager.Close(context.Background())
					return nil, fmt.Errorf("register lsp_%s: %w", kind, err)
				}
			}
			app.LSP = manager
			app.lifecycleHooks.SetPostToolObserver(manager.PostEditDiagnostics)
		}
	}
	if app.SkillRegistry != nil {
		available := func(name string) bool {
			for _, candidate := range app.Capabilities.ToolNames {
				if candidate == name {
					return true
				}
			}
			return false
		}
		if err := registry.Register(SkillListTool{Skills: app.SkillRegistry, Tools: registry, Available: available}); err != nil {
			return nil, fmt.Errorf("register skill_list: %w", err)
		}
		if err := registry.Register(SkillLoadTool{Skills: app.SkillRegistry, Tools: registry, Available: available}); err != nil {
			return nil, fmt.Errorf("register skill_load: %w", err)
		}
	}
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild {
		for _, tool := range []Tool{SkillCreateTool{Scope: scope}, SkillTestTool{Scope: scope}, SkillEnableTool{Scope: scope, Skills: app.SkillRegistry}} {
			if err := registry.Register(tool); err != nil {
				return nil, fmt.Errorf("register skill builder tool %q: %w", tool.Definition().Name, err)
			}
		}
	}
	if app.PluginRegistry != nil {
		if err := registry.Register(PluginListTool{Plugins: app.PluginRegistry}); err != nil {
			return nil, fmt.Errorf("register plugin_list: %w", err)
		}
	}
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild {
		for _, tool := range []Tool{PluginCreateTool{Scope: scope}, PluginTestTool{Scope: scope}, PluginEnableTool{Scope: scope, Plugins: app.PluginRegistry}} {
			if err := registry.Register(tool); err != nil {
				return nil, fmt.Errorf("register plugin builder tool %q: %w", tool.Definition().Name, err)
			}
		}
	}
	// Local Git mutation is an ordinary parent-only capability. Locked and
	// provider-child compositions retain the existing read-only Git surface.
	if options.Policy != AssistantPolicyLocked && app.ProviderRole != ProviderRoleChild {
		for _, tool := range []Tool{GitStagedReviewTool{Scope: scope}, GitStageTool{Scope: scope}, GitCommitTool{Scope: scope}} {
			if err := registry.Register(tool); err != nil {
				return nil, fmt.Errorf("register bounded Git tool: %w", err)
			}
		}
		if err := registry.Register(RepoCloneTool{Scope: scope}); err != nil {
			return nil, fmt.Errorf("register repository clone tool: %w", err)
		}
	}
	if runtimeTool, ok := registry.Lookup("runtime_info"); ok {
		if runtimeInfo, ok := runtimeTool.(*RuntimeInfoTool); ok {
			runtimeInfo.Application = app
		}
	}
	if currentTimeTool, ok := registry.Lookup(CurrentTimeToolName); ok {
		if currentTime, ok := currentTimeTool.(*CurrentTimeTool); ok {
			currentTime.DefaultTimeZone = config.Web.EffectiveTimeZone()
		}
	}
	if config.Terminal.Enabled {
		terminalManager := options.TerminalManager
		if terminalManager == nil {
			terminalManager, err = NewTerminalManager(scope, config.Terminal)
			if err != nil {
				return nil, fmt.Errorf("configure terminal: %w", err)
			}
		}
		terminalManager.SetAuditHook(options.TerminalAuditHook)
		if err := registry.Register(TerminalTool{Manager: terminalManager}); err != nil {
			return nil, fmt.Errorf("register terminal tool: %w", err)
		}
		app.Terminal = terminalManager
	}
	if options.Policy != AssistantPolicyLocked {
		// The foreground shell authority is intentionally a distinct composition.
		// Trusted policy allows the write-root sandbox directly; prompted policy
		// routes it through approval. Keep legacy run_command for internal/child
		// callers, but not as a model-facing fallback.
		_ = registry.Unregister("run_command")
		_ = registry.Unregister(ExecToolName)
		_ = registry.Unregister(ExecReadOnlyToolName)
		_ = registry.Unregister(HostExecToolName)
		if scope.EnforceWriteRoots {
			if err := registry.Register(ExecReadOnlyTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
				return nil, fmt.Errorf("register exec_readonly tool: %w", err)
			}
		}
		if err := registry.Register(ExecTool{Scope: scope, Terminal: app.Terminal, ArtifactStore: artifactStore}); err != nil {
			return nil, fmt.Errorf("register exec tool: %w", err)
		}
		if err := registry.Register(HostExecTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
			return nil, fmt.Errorf("register host exec tool: %w", err)
		}
	}
	if app.ConversationHistory != nil {
		if err := registry.Register(ConversationHistorySearchTool{Service: app.ConversationHistory, Actor: actor}); err != nil {
			return nil, fmt.Errorf("register conversation history tool: %w", err)
		}
	}
	if options.Policy != AssistantPolicyLocked {
		if err := registry.Register(ScratchSetTool{Store: scratchpadStore}); err != nil {
			return nil, fmt.Errorf("register scratch_set: %w", err)
		}
		if err := registry.Register(ScratchGetTool{Store: scratchpadStore}); err != nil {
			return nil, fmt.Errorf("register scratch_get: %w", err)
		}
		if err := registry.Register(ScratchListTool{Store: scratchpadStore}); err != nil {
			return nil, fmt.Errorf("register scratch_list: %w", err)
		}
		if err := registry.Register(ScratchDeleteTool{Store: scratchpadStore}); err != nil {
			return nil, fmt.Errorf("register scratch_delete: %w", err)
		}
		if systemInspector != nil && runtime.GOOS == "linux" {
			for _, tool := range []Tool{ProcessListTool{Inspector: systemInspector}, ProcessInfoTool{Inspector: systemInspector}} {
				if err := registry.Register(tool); err != nil {
					return nil, fmt.Errorf("register process inspector tool: %w", err)
				}
			}
			serviceAvailable := systemInspectorProvided
			if capability, ok := systemInspector.(interface{ ServiceInspectionAvailable() bool }); ok {
				serviceAvailable = capability.ServiceInspectionAvailable()
			}
			if serviceAvailable {
				for _, tool := range []Tool{ServiceListTool{Inspector: systemInspector}, ServiceStatusTool{Inspector: systemInspector}, ServiceLogsTool{Inspector: systemInspector}} {
					if err := registry.Register(tool); err != nil {
						return nil, fmt.Errorf("register service inspector tool: %w", err)
					}
				}
			}
		}
		reconReview, reconErr := NewReconReviewService(app)
		if reconErr == nil {
			if err := registry.Register(ReconReviewTool{Service: reconReview}); err != nil {
				return nil, fmt.Errorf("register recon review tool: %w", err)
			}
			app.ReconReview = reconReview
		}
		if app.Specialist != nil && app.Specialist.isConfiguredLocalChild() {
			verification := NewClaimVerificationService(app.Specialist)
			if err := registry.Register(VerifyClaimsTool{Service: verification}); err != nil {
				return nil, fmt.Errorf("register verify claims tool: %w", err)
			}
			app.ClaimVerification = verification
		}
	}
	policy := GeneralAssistantPolicy{Mode: options.Policy}
	if options.Policy != AssistantPolicyLocked {
		app.Executor = &Executor{Registry: registry, Scope: scope, Policy: policy, ApprovalMode: options.ApprovalMode, Approval: options.Approval, Auditor: options.Auditor}
		if config.Assistant.RequirePlan {
			app.Executor.planGate = &PlanGate{AllowUnclassifiedBeforePlan: config.Assistant.AllowUnclassifiedBeforePlan}
		}
	}
	app.RequirePlan = options.Policy != AssistantPolicyLocked && config.Assistant.RequirePlan
	if app.Executor != nil && options.Policy != AssistantPolicyLocked && config.Web.Scout.Enabled {
		if app.WebResearch == nil {
			return nil, errors.New("web scout requires the raw web executor")
		}
		scout, scoutErr := NewWebResearchScoutService(app, app.WebResearch, config.Web.Scout)
		if scoutErr != nil {
			return nil, fmt.Errorf("configure web scout: %w", scoutErr)
		}
		app.ForegroundWebResearch, scoutErr = scout.ForegroundExecutor()
		if scoutErr != nil {
			return nil, fmt.Errorf("configure web scout executor: %w", scoutErr)
		}
	}
	if app.Executor != nil && app.Specialist != nil && app.Specialist.isConfiguredLocalChild() {
		delegateScope := app.Executor.Scope
		delegateScope.TimeoutSeconds = MaxDelegateReconTimeoutSeconds
		if delegate, delegateErr := NewDelegateReconService(app, delegateScope); delegateErr == nil {
			if err := registry.Register(DelegateReconTool{Service: delegate}); err != nil {
				return nil, fmt.Errorf("register delegate recon tool: %w", err)
			}
			app.DelegateRecon = delegate
		}
	}
	if app.Executor != nil && (config.Coding.Enabled || options.CodingLocalExecutor != nil || strings.TrimSpace(options.CodingProjectRoot) != "") {
		codingRegistry, codingErr := NewCodingBackendRegistry(config.Coding)
		if codingErr != nil {
			return nil, fmt.Errorf("coding delegation registry: %w", codingErr)
		}
		codingWorkspace := options.CodingWorkspace
		codingProjectRoot := strings.TrimSpace(options.CodingProjectRoot)
		if codingProjectRoot != "" {
			resolvedProjectRoot, resolveErr := existingResolvedDir(codingProjectRoot)
			if resolveErr != nil {
				return nil, fmt.Errorf("coding project root: %w", resolveErr)
			}
			if !scopeWithin(scope.Root, resolvedProjectRoot) {
				return nil, errors.New("coding project root must remain within assistant scope")
			}
			codingProjectRoot = resolvedProjectRoot
		}
		if codingWorkspace == nil && codingProjectRoot != "" {
			managedRoot := strings.TrimSpace(options.CodingManagedRoot)
			if managedRoot == "" {
				managedRoot, codingErr = defaultCodingManagedRoot(codingProjectRoot)
				if codingErr != nil {
					return nil, fmt.Errorf("coding managed root: %w", codingErr)
				}
			}
			codingWorkspace, codingErr = NewGitWorkspaceManager(codingProjectRoot, managedRoot, options.CodingTrace)
			if codingErr != nil {
				return nil, fmt.Errorf("coding workspace: %w", codingErr)
			}
		}
		if codingProjectRoot == "" {
			if provider, ok := codingWorkspace.(interface{ RepositoryRoot() string }); ok {
				codingProjectRoot = strings.TrimSpace(provider.RepositoryRoot())
				if codingProjectRoot != "" && !scopeWithin(scope.Root, codingProjectRoot) {
					return nil, errors.New("configured coding workspace repository must remain within assistant scope")
				}
			}
		}
		if strings.TrimSpace(options.CodingBaseCommit) == "" && codingProjectRoot != "" {
			head, headErr := runGit(context.Background(), codingProjectRoot, "rev-parse", "HEAD")
			if headErr != nil {
				return nil, fmt.Errorf("coding base commit: %w", headErr)
			}
			options.CodingBaseCommit = strings.TrimSpace(head.Stdout)
			if options.CodingBaseCommit == "" {
				return nil, errors.New("coding base commit is empty")
			}
		}
		codingService, codingErr := NewCodingDelegationService(codingRegistry, scope, codingWorkspace, options.CodingBaseCommit)
		if codingErr != nil {
			return nil, codingErr
		}
		if strings.TrimSpace(options.CodingParentID) != "" {
			codingService.ParentID = options.CodingParentID
		}
		codingService.Trace = options.CodingTrace
		codingLocalExecutor := options.CodingLocalExecutor
		if codingLocalExecutor == nil && config.Coding.Enabled && app.Executor != nil {
			codingLocalExecutor, codingErr = newCodingLocalExecutor(app)
			if codingErr != nil {
				return nil, fmt.Errorf("coding local executor: %w", codingErr)
			}
		}
		codingService.LocalExecutor = codingLocalExecutor
		codingService.ProjectRoot = codingProjectRoot
		if err := app.Executor.Registry.Register(NewCodingDelegationTool(codingService)); err != nil {
			return nil, fmt.Errorf("register coding delegation tool: %w", err)
		}
		app.CodingDelegation = codingService
	}
	operationsDelegation := options.OperationsDelegation
	if operationsDelegation == nil && config.Operations.Enabled {
		operationsDelegation, err = newConfiguredOperationsDelegation(config.Operations, scope)
		if err != nil {
			return nil, fmt.Errorf("configure operations delegation: %w", err)
		}
	}
	if app.Executor != nil && operationsDelegation != nil {
		if err := app.Executor.Registry.Register(NewOperationsDelegationTool(operationsDelegation)); err != nil {
			return nil, fmt.Errorf("register operations delegation tool: %w", err)
		}
		app.OperationsDelegation = operationsDelegation
	}
	app.MemoryService = memoryService
	mood, moodErr := NewMoodManager(memoryRoot)
	if moodErr != nil {
		return nil, fmt.Errorf("assistant mood: %w", moodErr)
	}
	app.Mood = mood
	dashboard, dashboardErr := NewMemoryDashboard(memoryRoot)
	if dashboardErr != nil {
		return nil, fmt.Errorf("assistant memory dashboard: %w", dashboardErr)
	}
	app.Dashboard = dashboard
	if config.Assistant.RuntimeFactsEnabled() {
		facts := CurrentHostRuntimeFacts()
		app.RuntimeFacts = &facts
	}
	app.ScratchpadStore = scratchpadStore
	app.SystemInspector = systemInspector
	app.ArtifactStore = artifactStore
	app.Media = NewMediaService(artifactStore)
	if config.Web.MediaSearchEnabled && options.EnableMediaSearch {
		webClient, webErr := NewWebClient(config.Web)
		if webErr != nil {
			return nil, fmt.Errorf("configure media search: %w", webErr)
		}
		app.MediaSearch = NewMediaSearchService(webClient)
		if app.Executor != nil {
			if err := app.Executor.Registry.Register(MediaSearchTool{Service: app.MediaSearch}); err != nil {
				return nil, fmt.Errorf("register media search tool: %w", err)
			}
			if err := app.Executor.Registry.Register(MediaRetrieveTool{Search: app.MediaSearch, Media: app.Media}); err != nil {
				return nil, fmt.Errorf("register media retrieve tool: %w", err)
			}
		}
	}
	app.configureProviderAttachmentResolver()
	app.AttachmentStore = attachmentStore
	if app.Specialist != nil {
		app.Specialist.SetAttachmentResolver(artifactStore)
	}
	app.MemoryActor = actor
	app.MemoryPolicy = memoryPolicy
	app.MemoryRetrieval = memoryRetrieval
	app.MemoryRecallMode = config.Memory.RecallMode.effective()
	app.MemoryFailure = MemoryFailureOptional
	// Passive time awareness belongs to the ordinary durable parent only.
	// Child/storeless and compatibility agents retain their existing transcript.
	app.TimeSyncEnabled = app.ProviderRole != ProviderRoleChild
	app.TimeSyncTimeZone = config.Web.EffectiveTimeZone()
	app.RemoteGrant = options.RemoteGrant
	// Tools the foreground assistant no longer exposes; see
	// prunedForegroundTools. Their implementations and other users (child
	// and coding registries, tests) are unchanged.
	for _, name := range prunedForegroundTools {
		_ = registry.Unregister(name)
	}
	app.Capabilities = AssistantCapabilities{ScopeRoot: root, ScopeRoots: append([]string(nil), roots...), WriteRoot: writeRoots[0], WriteRoots: append([]string(nil), writeRoots...), FullMachine: fullMachine, Policy: options.Policy, WebEnabled: options.Policy != AssistantPolicyLocked && config.Web.Enabled, RemoteGranted: options.RemoteGrant != nil, LaunchRoot: launchRoot}
	for _, skill := range enabledSkills {
		app.Capabilities.SkillNames = append(app.Capabilities.SkillNames, skill.Manifest.Name)
	}
	for _, plugin := range enabledPlugins {
		app.Capabilities.PluginNames = append(app.Capabilities.PluginNames, plugin.Manifest.Name)
	}
	if options.Policy != AssistantPolicyLocked {
		for _, definition := range registry.Definitions() {
			app.Capabilities.ToolNames = append(app.Capabilities.ToolNames, definition.Name)
		}
		if app.ProviderRole != ProviderRoleChild {
			app.Capabilities.ToolNames = append(app.Capabilities.ToolNames, PlanToolName, PlanStatusToolName, PlanUpdateToolName)
		}
		if app.Executor != nil && app.ProviderRole != ProviderRoleChild {
			app.Capabilities.ToolNames = append(app.Capabilities.ToolNames, ContextStatusToolName)
		}
		webExecutor := app.WebResearch
		if app.ForegroundWebResearch != nil {
			webExecutor = app.ForegroundWebResearch
		}
		if webExecutor != nil && webExecutor.Registry != nil {
			for _, definition := range webExecutor.Registry.Definitions() {
				app.Capabilities.ToolNames = append(app.Capabilities.ToolNames, definition.Name)
			}
		}
	}
	if options.DiagnosticSink != nil {
		app.Diagnostics = options.DiagnosticSink
	} else if !options.DisableDefaultDiagnostics {
		path, pathErr := DefaultBeeRuntimeDiagnosticsPath()
		if pathErr != nil {
			return nil, fmt.Errorf("assistant diagnostics path: %w", pathErr)
		}
		sink, sinkErr := NewJSONLDiagnosticSink(path)
		if sinkErr != nil {
			return nil, fmt.Errorf("assistant diagnostics: %w", sinkErr)
		}
		app.Diagnostics = sink
		app.ownsDiagnostics = true
	}
	return app, nil
}

func defaultCodingManagedRoot(projectRoot string) (string, error) {
	stateRoot, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(projectRoot)
	digest := sha256.Sum256([]byte(clean))
	name := filepath.Base(clean) + "-" + hex.EncodeToString(digest[:6])
	return filepath.Join(stateRoot, "coding-workspaces", name), nil
}

func newCodingLocalExecutor(app *Application) (ChildExecutor, error) {
	runner, err := NewChildRunner(app)
	if err != nil {
		return nil, err
	}
	if app == nil || app.Executor == nil {
		return runner, nil
	}
	// Ordinary foreground commands intentionally default to a short 30-second
	// executor scope. Coding is still parent-owned, but a bounded coding task
	// needs its own explicit ceiling so a local fallback is not rejected before
	// it starts. This does not widen the filesystem root, tools, policy, or
	// provider authority copied into the child.
	limits := CodingDelegationLimits{}.withDefaults()
	copy := *app.Executor
	scope := copy.Scope
	if scope.TimeoutSeconds <= 0 || scope.TimeoutSeconds < limits.MaxTimeoutSeconds {
		scope.TimeoutSeconds = limits.MaxTimeoutSeconds
	}
	if scope.MaxOutputBytes <= 0 || scope.MaxOutputBytes < limits.MaxOutputBytes {
		scope.MaxOutputBytes = limits.MaxOutputBytes
	}
	copy.Scope = scope
	runner.Executor = &copy
	requirePlan := false
	runner.RequirePlanOverride = &requirePlan
	return runner, nil
}

// NewGeneralAssistant is a convenience alias for callers that want a
// composition-root-shaped constructor without remembering the longer name.
func NewGeneralAssistant(config Config, profile string, options AssistantCompositionOptions) (*Application, error) {
	return ComposeGeneralAssistant(config, profile, options)
}

func (app *Application) toolAllowed(name string) bool {
	if app == nil || app.toolAllowlist == nil {
		return true
	}
	_, ok := app.toolAllowlist[name]
	return ok
}

// RestrictTools narrows an already-composed application to an exact immutable
// allowlist. It is used by configured recurring jobs before their first turn;
// it never widens composition and rejects unknown or partial internal groups.
func (app *Application) RestrictTools(allowed []string) error {
	if app == nil || app.Executor == nil || app.Executor.Registry == nil {
		return errors.New("application executor registry is required")
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if !validMCPName(name) {
			return fmt.Errorf("tool allowlist contains invalid name %q", name)
		}
		if _, exists := allow[name]; exists {
			return fmt.Errorf("tool allowlist repeats %q", name)
		}
		allow[name] = struct{}{}
	}
	planCount := 0
	for _, name := range []string{PlanToolName, PlanStatusToolName, PlanUpdateToolName} {
		if _, ok := allow[name]; ok {
			planCount++
		}
	}
	if planCount != 0 && planCount != 3 {
		return errors.New("tool allowlist must include all planning tools or none")
	}
	if app.RequirePlan && planCount != 3 {
		return errors.New("tool allowlist must include all planning tools when plan mode is required")
	}
	available := make(map[string]struct{})
	registries := []*Registry{app.Executor.Registry}
	if app.ForegroundWebResearch != nil {
		registries = append(registries, app.ForegroundWebResearch.Registry)
	} else if app.WebResearch != nil {
		registries = append(registries, app.WebResearch.Registry)
	}
	for _, registry := range registries {
		if registry == nil {
			continue
		}
		for _, definition := range registry.Definitions() {
			available[definition.Name] = struct{}{}
		}
	}
	if app.ProviderRole != ProviderRoleChild {
		available[PlanToolName] = struct{}{}
		available[PlanStatusToolName] = struct{}{}
		available[PlanUpdateToolName] = struct{}{}
		available[ContextStatusToolName] = struct{}{}
	}
	for name := range allow {
		if _, ok := available[name]; !ok {
			return fmt.Errorf("configured tool %q is unavailable", name)
		}
	}
	seenRegistries := make(map[*Registry]struct{}, len(registries))
	for _, registry := range registries {
		if registry == nil {
			continue
		}
		if _, seen := seenRegistries[registry]; seen {
			continue
		}
		seenRegistries[registry] = struct{}{}
		for _, definition := range registry.Definitions() {
			if _, ok := allow[definition.Name]; !ok {
				_ = registry.Unregister(definition.Name)
			}
		}
	}
	app.toolAllowlist = allow
	app.Capabilities.ToolNames = append([]string(nil), allowed...)
	return nil
}

func resolveAssistantScope(config AssistantConfig, override string, fullMachine bool) (string, bool, error) {
	roots, fullMachine, err := resolveAssistantScopes(config, override, nil, fullMachine)
	if err != nil {
		return "", false, err
	}
	return roots[0], fullMachine, nil
}

func resolveAssistantScopes(config AssistantConfig, override string, overrides []string, fullMachine bool) ([]string, bool, error) {
	if config.FullMachine {
		fullMachine = true
	}
	if fullMachine {
		if filepath.Separator == '\\' {
			return []string{filepath.VolumeName(os.Getenv("SystemDrive") + string(filepath.Separator))}, true, nil
		}
		return []string{string(filepath.Separator)}, true, nil
	}
	inputs := append([]string(nil), overrides...)
	if len(inputs) == 0 && strings.TrimSpace(override) != "" {
		inputs = []string{override}
	}
	if len(inputs) == 0 && len(config.ScopeRoots) > 0 {
		inputs = append(inputs, config.ScopeRoots...)
	}
	if len(inputs) == 0 && strings.TrimSpace(config.ScopeRoot) != "" {
		inputs = []string{config.ScopeRoot}
	}
	if len(inputs) == 0 {
		var err error
		inputs, err = defaultAssistantScopeRoots()
		if err != nil {
			return nil, false, err
		}
	}
	scope := ExecutionScope{Root: inputs[0], Roots: append([]string(nil), inputs[1:]...)}
	if err := scope.Validate(); err != nil {
		return nil, false, fmt.Errorf("assistant scope: %w", err)
	}
	resolved, err := scope.rootPaths()
	return resolved, false, err
}

// defaultAssistantScopeRoots aligns relative filesystem paths with BEE.md's
// workspace conventions without reducing Bee's historical home-directory
// access. An uninitialized installation retains the legacy home-only default;
// scope resolution never creates directories as a side effect.
func defaultAssistantScopeRoots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find assistant scope root: %w", err)
	}
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		return nil, fmt.Errorf("find Bee workspace scope root: %w", err)
	}
	info, err := os.Stat(workspace)
	if errors.Is(err, os.ErrNotExist) {
		return []string{home}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Bee workspace scope root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("Bee workspace scope root must be a directory")
	}
	return []string{workspace, home}, nil
}

func resolveAssistantWriteScopes(config AssistantConfig, override string, overrides, readRoots []string, fullMachine bool) ([]string, error) {
	inputs := append([]string(nil), overrides...)
	if len(inputs) == 0 && strings.TrimSpace(override) != "" {
		inputs = []string{override}
	}
	if len(inputs) == 0 && len(config.WriteRoots) > 0 {
		inputs = append(inputs, config.WriteRoots...)
	}
	if len(inputs) == 0 && strings.TrimSpace(config.WriteRoot) != "" {
		inputs = []string{config.WriteRoot}
	}
	if len(inputs) == 0 {
		inputs = []string{readRoots[0]}
		if fullMachine {
			workspace, workspaceErr := DefaultBeeWorkspaceRoot()
			if workspaceErr != nil {
				return nil, workspaceErr
			}
			if info, statErr := os.Stat(workspace); statErr == nil && info.IsDir() {
				inputs = []string{workspace}
			} else if statErr != nil {
				return nil, errors.New("full-machine reads require an initialized Bee workspace or explicit assistant.write_root/write_roots")
			} else {
				return nil, errors.New("Bee workspace write root must be a directory")
			}
		}
	}
	scope := ExecutionScope{Root: readRoots[0], Roots: append([]string(nil), readRoots[1:]...), WriteRoot: inputs[0], WriteRoots: append([]string(nil), inputs[1:]...)}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("assistant write scope: %w", err)
	}
	return scope.writeRootPaths()
}

// GeneralAssistantPolicy applies the universal policy. Trusted mode avoids
// ordinary per-write prompts, while intrinsically guarded self-extension
// actions still require approval. The explicit trusted exec/terminal process
// surface bypasses the coding-plan gate. Prompted mode asks through the
// supplied ApprovalHook; locked mode exposes no tools.
type GeneralAssistantPolicy struct{ Mode AssistantPolicyMode }

func (policy GeneralAssistantPolicy) Evaluate(_ context.Context, definition ToolDefinition, args json.RawMessage, scope ExecutionScope) (PolicyDecision, error) {
	if policy.Mode == AssistantPolicyLocked {
		return PolicyDeny, nil
	}
	if definition.Name == SkillEnableToolName || definition.Name == PluginEnableToolName {
		// Self-extension activation is never silently trusted. The tested package
		// checksum is present in the approval arguments and execution fails closed
		// when the interface has no human approval hook.
		return PolicyRequireApproval, nil
	}
	if definition.Name == ExecToolName {
		if !scope.TrustedForeground {
			return PolicyDeny, nil
		}
		if policy.Mode == AssistantPolicyPrompted {
			return PolicyRequireApproval, nil
		}
		return PolicyAllow, nil
	}
	if definition.Name == HostExecToolName {
		if !scope.TrustedForeground {
			return PolicyDeny, nil
		}
		return PolicyRequireApproval, nil
	}
	if needsApproval, handled, err := assistantMutationPathApproval(definition.Name, args, scope); err != nil {
		return PolicyDeny, err
	} else if handled && needsApproval {
		return PolicyRequireApproval, nil
	}
	if definition.Name == "run_command" {
		var input runArgs
		if err := parseArgs(args, &input); err == nil && (input.Target == CommandTargetLocal || input.Target == CommandTargetSSH) {
			if input.Target == CommandTargetSSH && scope.TrustedForeground && scope.RemoteGrant == nil {
				// Trusted foreground SSH is an ordinary tool call. The tool itself
				// remains authoritative for exact host/user/port validation.
			} else if scope.RemoteGrant == nil || scope.RemoteGrant.permits(commandRequest{Target: input.Target, Host: input.Host, User: input.User, Port: input.Port, Elevated: input.Elevated, Stdin: input.Stdin}, nowUTC()) != nil {
				return PolicyDeny, nil
			}
		}
		if err := parseArgs(args, &input); err == nil && input.Target == CommandTargetWorkspace && input.Elevated {
			return PolicyDeny, nil
		}
	}
	if definition.Mutability == ToolMutabilityReadOnly {
		return PolicyAllow, nil
	}
	if policy.Mode == AssistantPolicyPrompted {
		return PolicyRequireApproval, nil
	}
	return PolicyAllow, nil
}

func assistantMutationPathApproval(name string, args json.RawMessage, scope ExecutionScope) (needsApproval, handled bool, err error) {
	// Executor validates real scopes before policy evaluation. Keeping the
	// zero-value policy seam path-agnostic preserves standalone policy checks.
	if strings.TrimSpace(scope.Root) == "" {
		return false, false, nil
	}
	path, allowMissing, handled, err := assistantMutationTarget(name, args)
	if err != nil || !handled {
		return false, handled, err
	}
	needsApproval, err = pathNeedsWriteApproval(scope, path, allowMissing)
	return needsApproval, true, err
}

// assistantMutationTarget extracts the path a path-mutating tool call
// writes, and whether that path may not exist yet.
func assistantMutationTarget(name string, args json.RawMessage) (path string, allowMissing, handled bool, err error) {
	switch name {
	case "write_file":
		var input writeArgs
		if err := parseArgs(args, &input); err != nil {
			return "", false, true, fmt.Errorf("write_file arguments: %w", err)
		}
		operation, err := validateWriteArgs(input)
		if err != nil {
			return "", false, true, fmt.Errorf("write_file arguments: %w", err)
		}
		path = input.Path
		allowMissing = operation == writeOperationCreate || operation == writeOperationAppend
	case "edit_file":
		var input editArgs
		if err := parseArgs(args, &input); err != nil {
			return "", false, true, fmt.Errorf("edit_file arguments: %w", err)
		}
		path = input.Path
	case "git_stage":
		var input gitStageArgs
		if err := parseArgs(args, &input); err != nil {
			return "", false, true, errors.New("git_stage arguments are invalid")
		}
		path = input.RepoPath
	case "git_commit":
		var input gitCommitArgs
		if err := parseArgs(args, &input); err != nil {
			return "", false, true, errors.New("git_commit arguments are invalid")
		}
		path = input.RepoPath
	default:
		return "", false, false, nil
	}
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	return path, allowMissing, true, nil
}

// NewGeneralAssistantRegistry returns the root-bounded local tools plus
// bounded memory lookup and explicit durable-memory write tools.
func NewGeneralAssistantRegistry(scope ExecutionScope, memory MemoryService, actor MemoryActor, policy MemoryPolicy, jobStores ...*JobStore) (*Registry, error) {
	var jobStore *JobStore
	if len(jobStores) > 0 {
		jobStore = jobStores[0]
	}
	return newGeneralAssistantRegistry(scope, memory, actor, policy, MemoryRetrievalOptions{}, jobStore, nil, nil, nil)
}

func newGeneralAssistantRegistry(scope ExecutionScope, memory MemoryService, actor MemoryActor, policy MemoryPolicy, retrieval MemoryRetrievalOptions, jobStore *JobStore, artifactStore *ArtifactStore, todoStore *TodoStore, journal *MutationJournal, rerankers ...*MemoryReranker) (*Registry, error) {
	registry, err := NewCodingRegistry(scope)
	if err != nil {
		return nil, err
	}
	if artifactStore != nil {
		_ = registry.Unregister("run_command")
		if err := registry.Register(RunCommandTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
			return nil, err
		}
		if err := registry.Register(ArtifactReadTool{Store: artifactStore}); err != nil {
			return nil, err
		}
	}
	if scope.TrustedForeground {
		_ = registry.Unregister("run_command")
		_ = registry.Unregister(ExecToolName)
		_ = registry.Unregister(ExecReadOnlyToolName)
		if scope.EnforceWriteRoots {
			if err := registry.Register(ExecReadOnlyTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
				return nil, err
			}
		}
		if err := registry.Register(ExecTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
			return nil, err
		}
		if err := registry.Register(HostExecTool{Scope: scope, ArtifactStore: artifactStore}); err != nil {
			return nil, err
		}
	}
	if memory == nil {
		return nil, errors.New("memory service is required")
	}
	if _, err := registerRuntimeInfoTool(registry, scope); err != nil {
		return nil, err
	}
	if err := registry.Register(&CurrentTimeTool{DefaultTimeZone: DefaultWebTimeZone}); err != nil {
		return nil, err
	}
	var reranker *MemoryReranker
	if len(rerankers) > 0 {
		reranker = rerankers[0]
	}
	for _, tool := range []Tool{
		MemorySearchTool{Service: memory, Actor: actor, Policy: policy, Retrieval: retrieval, Reranker: reranker},
		MemoryShowTool{Service: memory, Actor: actor, Policy: policy},
		MemoryRememberTool{Service: memory, Actor: actor},
		MemoryForgetTool{Service: memory, Actor: actor},
		GitStatusTool{Scope: scope},
		GitDiffTool{Scope: scope},
	} {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	if _, ok := memory.(MemoryFieldEditor); ok {
		if err := registry.Register(MemoryEditTool{Service: memory, Actor: actor}); err != nil {
			return nil, err
		}
	}
	if _, ok := memory.(MemorySuperseder); ok {
		if err := registry.Register(MemorySupersedeTool{Service: memory, Actor: actor}); err != nil {
			return nil, err
		}
	}
	if jobStore != nil {
		for _, tool := range NewJobTools(jobStore) {
			if err := registry.Register(tool); err != nil {
				return nil, err
			}
		}
	}
	if todoStore != nil {
		for _, tool := range NewTodoTools(todoStore) {
			if err := registry.Register(tool); err != nil {
				return nil, err
			}
		}
	}
	if journal != nil {
		_ = registry.Unregister("write_file")
		if err := registry.Register(WriteFileTool{Scope: scope, Journal: journal}); err != nil {
			return nil, err
		}
		_ = registry.Unregister("edit_file")
		if err := registry.Register(EditFileTool{Scope: scope, Journal: journal}); err != nil {
			return nil, err
		}
		for _, tool := range []Tool{MutationListTool{Journal: journal}, MutationShowTool{Journal: journal}, MutationUndoTool{Journal: journal}} {
			if err := registry.Register(tool); err != nil {
				return nil, err
			}
		}
	}
	if artifactStore != nil {
		if err := registry.Register(CreateArtifactTool{Store: artifactStore}); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// EligibleLaunchRoot decides whether the directory Bee was started from may
// become an extra write root. It returns the cleaned, symlink-resolved
// directory and an empty reason when it should be added; otherwise a short
// reason. "already writable" is not a refusal, just a no-op.
func EligibleLaunchRoot(dir string, writeRoots []string) (string, string) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return "", "unresolvable"
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	abs = filepath.Clean(abs)
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return "", "not a directory"
	}
	if abs == string(filepath.Separator) {
		return "", "the filesystem root"
	}
	for _, root := range writeRoots {
		if filepath.Clean(root) == abs || scopeWithin(root, abs) {
			return "", "already writable"
		}
	}
	// Making these writable would expose far more than a project directory:
	// all of home, Bee's own config and secrets, or the session transcripts.
	type guardedPath struct{ path, label string }
	var guarded []guardedPath
	add := func(resolve func() (string, error), label string) {
		if path, err := resolve(); err == nil {
			guarded = append(guarded, guardedPath{resolvedOrSelf(path), label})
		}
	}
	add(os.UserHomeDir, "the home directory")
	add(DefaultBeeHome, "Bee's home directory")
	for _, root := range SessionStoreRoots() {
		root := root
		add(func() (string, error) { return root, nil }, "the session transcript store")
	}
	add(DefaultBeeSecretsRoot, "Bee's secrets directory")
	add(DefaultBeeConfigPath, "Bee's config file")
	for _, item := range guarded {
		if abs == item.path || scopeWithin(abs, item.path) {
			return "", "it is or contains " + item.label
		}
	}
	if beeHome, homeErr := DefaultBeeHome(); homeErr == nil && scopeWithin(resolvedOrSelf(beeHome), abs) {
		return "", "it is inside Bee's home directory"
	}
	// The session store can live outside Bee's home (legacy XDG installs,
	// BEE_SESSION_ROOT).
	for _, sessions := range SessionStoreRoots() {
		if scopeWithin(resolvedOrSelf(sessions), abs) {
			return "", "it is inside the session transcript store"
		}
	}
	return abs, ""
}

// prunedForegroundTools are not offered to the main assistant model. Chosen
// on 2026-11-12 from 90 real sessions (5,541 calls): each was never or
// almost never called and is covered elsewhere, and a smaller tool list is
// easier for a local model to choose from. Restoring one is a one-line
// removal here.
//   - current_time: every turn already carries a clock record.
//   - scratch_*: overlaps memory and todos; 3 calls ever.
//   - process_list/process_info: ps via exec_readonly.
//   - git_*: all git work goes through exec.
//   - plugin_*: skills are authored, plugins are not; installed plugin
//     tools are unaffected.
//   - job_show/job_recover/job_approve: job_list covers show, recovery is an
//     operator action, and approving a scheduled job is the human's call.
//   - todo_show/todo_archive, mutation_show: covered by the list tools.
//   - verify_claims, delegate_recon (2026-11-16): register once the child
//     counts as local; both hand reading or checking to the child, which
//     no longer pays off now that the main model is the faster one (the
//     same reason the web scout stays off).
var prunedForegroundTools = []string{
	"current_time",
	"scratch_get", "scratch_set", "scratch_list", "scratch_delete",
	"process_list", "process_info",
	"git_status", "git_diff", "git_staged_review", "git_stage", "git_commit",
	"plugin_create", "plugin_enable", "plugin_test", "plugin_list",
	"job_show", "job_recover", "job_approve",
	"todo_show", "todo_archive",
	"mutation_show",
	VerifyClaimsToolName, DelegateReconToolName,
}
