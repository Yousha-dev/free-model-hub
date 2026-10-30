package admin

// This file locks the critical frontend invariants of the console page (key decoding, request gates, single-page layout, collapsible sections).
// The page and assertions are in the same package; modify this when changing the page rather than scattering tests to the plugin root.

import (
	"regexp"
	"strings"
	"testing"
)

// unwrapRe matches the "unwrap another envelope" pattern: .result not followed by s (distinguishing valid .results).
var unwrapRe = regexp.MustCompile(`\.result\s*[;,)\n]`)

// TestConsolePageDecodesPanelKey verifies that the console page can decode the management key from the CPA management panel's persisted snapshot.
//
// This page shares the same origin as the panel, so it can reuse the panel's saved key, avoiding duplicate user input. The panel
// (CPAMC) stores the entire authentication state under the localStorage key "cli-proxy-auth", with values being reversible
// XOR obfuscation:
//
//	Prefix "enc::v1::" + base64( utf8(JSON) XOR key )
//	key = utf8("cli-proxy-api-webui::secure-storage|<host>|<userAgent>")
//	JSON = {"state":{...,"managementKey":"<key>",...},"version":N}
//
// The key is derived from a public value, so same-origin pages can decode it. This asserts that the page actually implements this algorithm
// (if details differ from the panel, users will see 401 and the cause is extremely hard to locate).
func TestConsolePageDecodesPanelKey(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	// The obfuscation format and snapshot fields must match the panel.
	for _, literal := range []string{
		`"enc::v1::"`,                            // 前缀
		`"cli-proxy-api-webui::secure-storage"`,  // 种子
		`var PANEL_STORE_KEY = "cli-proxy-auth"`, // 面板的存储键名
		`var PANEL_KEY_FIELD = "managementKey"`,  // 密钥在快照里的字段名
		`localStorage.getItem(PANEL_STORE_KEY)`,  // 只读面板快照
		"snapshot.state",                         // zustand persist 的 {state,version}
		"location.host",                          // 密钥派生输入
		"navigator.userAgent",                    // 密钥派生输入
		"atob(",                                  // base64 解码
		"TextDecoder",                            // utf8 解码
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s (key decoding would break)", literal)
		}
	}

	// Old panel key names are no longer read: reading expired values would cause the page to hit probes with the wrong key,
	// wasting CPA's failure count quota for nothing.
	for _, legacy := range []string{
		`getItem("managementKey")`,
		"detectKey(",
		`localStorage.getItem("cpa-management-key")`,
	} {
		if strings.Contains(page, legacy) {
			t.Fatalf("console page must not read the legacy key source %s", legacy)
		}
	}

	// Keys must validate character sets: the panel's obfuscated key is derived from userAgent, and after browser upgrades it might
	// decode into garbled text; putting garbled text into the Authorization header will be rejected by the browser (Safari reports
	// "The string did not match the expected pattern."), making it extremely hard to locate.
	if !strings.Contains(page, "KEY_RE") {
		t.Fatal("console page must validate the key charset before using it in a header")
	}

	// Automatic reuse must be able to fall back to manual entry on failure, otherwise users will face a blank screen.
	for _, literal := range []string{`id="keyBox"`, `id="keyInput"`, `id="btnUseKey"`, "revealKeyInput"} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the manual key fallback (%s)", literal)
		}
	}
}

// TestConsolePageNeverCallsWithoutKey verifies that the console page sends no management requests before identity confirmation.
//
// CPA tracks management key failures by IP (**requests without a key also count**), and 5 failures block the entire
// /v0/management for 30 minutes. Earlier versions concurrently fired 5 first-screen requests when the key was unavailable,
// so "opening the plugin page" would directly lock out the backend. This test locks in three fixed invariants:
//
//  1. No key -> not a single request is sent;
//  2. Has key -> send only one probe, load the first screen only on success, and make the first screen serial;
//  3. Any 401/403 -> immediately stop all auto-refresh and polling timers.
func TestConsolePageNeverCallsWithoutKey(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	// 1) api() gate: no network traffic when unauthenticated/expired, and no network traffic without a key.
	for _, literal := range []string{
		"if (stopped) return Promise.resolve(null);",
		"if (!key) return Promise.resolve(null);",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the api() guard %q", literal)
		}
	}

	// 2) Single probe precedes the first screen; the first screen is a serial chain rather than concurrent rollout.
	if !regexp.MustCompile(`probeManagementAuth\(\)[\s\S]{0,800}startConsole\(\)`).MatchString(page) {
		t.Fatal("console page must validate the key with a single probe before loading data")
	}
	if !strings.Contains(page, `loadVendors().then(function () {`) {
		t.Fatal("console page must load the first screen serially (vendors first)")
	}

	// Timers are failure amplifiers: they can only start after key verification passes (authReady).
	if !strings.Contains(page, "if (authReady && box && box.checked)") {
		t.Fatal("log polling must require a verified key (authReady)")
	}
	if strings.Contains(page, "if (event.target.checked && !stopped)") {
		t.Fatal("log polling must not start from the raw stopped flag")
	}

	// 3) Authentication failure must stop all timers at once, not just stop logs.
	for _, literal := range []string{
		"function haltOnAuthFailure(status) {",
		"stopLoginPolling();",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the auth-failure stop %q", literal)
		}
	}

	// Manual input must also be verified via probe before being memorized, rather than writing to sessionStorage before trying;
	// Once verification passes, authReady must be set to true, otherwise log polling will never start.
	if !regexp.MustCompile(`probeManagementAuth\(\)[\s\S]{0,900}setItem\(MANUAL_STORE_KEY`).MatchString(page) {
		t.Fatal("manually entered keys must be probe-verified before being remembered")
	}
	if !regexp.MustCompile(`setItem\(MANUAL_STORE_KEY, value\)[\s\S]{0,200}authReady = true`).MatchString(page) {
		t.Fatal("manual login must mark authReady after the probe succeeds")
	}
}

// TestConsolePageDoesNotUnwrapHostBody verifies that the page does not mistakenly unwrap response fields.
//
// The host transparently passes the plugin's response body **as is** to the browser: the {ok, result} envelope is an RPC format
// between the plugin and the host, and never reaches the frontend. If the page unwraps another "result" layer, all business fields will
// become undefined — manifesting as "API returns 200 but interface is completely empty", which is extremely hard to locate.
func TestConsolePageDoesNotUnwrapHostBody(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	// Note: use precise regex matching: strings containing "data.result" will also match "data.results" (a valid field).
	if unwrapRe.MatchString(page) {
		t.Fatal("console page must not unwrap a second envelope layer (the host passes the body through verbatim)")
	}
	// Business fields must be accessed directly from the top level of the response.
	for _, field := range []string{
		"status.accounts", // 账号列表
		"payload.entries", // 日志
		"data.models",     // 模型清单
		"data.results",    // 额度/任务结果
	} {
		if !strings.Contains(page, field) {
			t.Fatalf("console page must read %q directly from the response body", field)
		}
	}
}

// TestConsolePageWiresHostOAuthLogin verifies that "Add Account" connects to the host's OAuth endpoint.
//
// The host provides a two-stage login for the plugin:
//
//	GET /v0/management/<provider>-auth-url?realm=...   → forwarded to auth.login.start
//	GET /v0/management/get-auth-status?state=...       → forward to auth.login.poll
//
// The page must use these endpoints (instead of the plugin's own routes), otherwise "Add Account" will be unavailable.
func TestConsolePageWiresHostOAuthLogin(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"/freehub-auth-url",  // 与 providerKey 拼接，宿主按此匹配 auth provider
		"get-auth-status",    // 宿主提供的状态轮询端点
		`id="addVendorMenu"`, // 供应商下拉菜单（替代原先的两个固定按钮）
		`id="btnAddAccount"`, // 添加账号入口
		"startLogin",         // 登录流程
		"pollLogin",          // 轮询流程
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the add-account wiring (%s)", literal)
		}
	}
	// These two APIs are under the **host root path** (/v0/management/<provider>-auth-url),
	// and cannot have a plugin prefix: the host extracts the provider by path, and having a prefix results in
	// "plugins/free-model-hub/workbuddy", causing validation to fail with a direct 404.
	if !strings.Contains(page, "hostPath(") {
		t.Fatal("host-level OAuth endpoints must not be prefixed with the plugin path")
	}
	if strings.Contains(page, `api("GET", "/freehub-auth-url`) {
		t.Fatal("auth-url must go through hostPath() (the host extracts the provider from the path)")
	}
	if !strings.Contains(page, `var HOST_MGMT = "/v0/management"`) {
		t.Fatal("console page must know the host management prefix")
	}

	// The provider must be optional: the page fetches the list from /vendors to populate the dropdown, and passes the vendor
	// as a parameter to the host during login (the host only recognizes the provider in the path, and the plugin determines the choice via parameters).
	if !strings.Contains(page, "loadVendors") {
		t.Fatal("console page must load the vendor list from the backend")
	}
	if !strings.Contains(page, "vendor=") {
		t.Fatal("console page must pass the selected vendor to the host login endpoint")
	}
}

// TestConsolePageIsSingleScrollLayout verifies that the page is a single-page scroll layout rather than tabs.
//
// Tabs hide quota, models, and tasks; the management page should show everything at a glance.
func TestConsolePageIsSingleScrollLayout(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	// There should no longer be navigation and view switching.
	for _, forbidden := range []string{"<nav>", "data-view=", "switchView", `id="view-accounts" hidden`} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must be a single scrolling page, found %q", forbidden)
		}
	}
	// All sections must be directly visible on the page (without hidden).
	for _, section := range []string{"Account Overview", "Quota", "Model", "Runtime Settings", "Run Logs"} {
		if !strings.Contains(page, section) {
			t.Fatalf("console page is missing the %q section", section)
		}
	}
	// Quota and models must have their own containers and buttons.
	for _, literal := range []string{
		`id="btnLoadQuotas"`, // 额度：按钮 + 并入账号表的列
		`id="models"`, `id="btnLoadModels"`, `id="modelFilter"`,
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s", literal)
		}
	}
	// The reward task center was removed together with its queue.
	for _, removed := range []string{`id="view-tasks"`, `id="queueProgress"`, "/tasks/", "/checkin"} {
		if strings.Contains(page, removed) {
			t.Fatalf("console page must not keep removed task UI (%s)", removed)
		}
	}

	// Quota no longer occupies a separate table: results are merged into the account table's "Remaining / Total" and "Usage Rate" columns.
	if strings.Contains(page, `id="quotas"`) {
		t.Fatal("quota must be merged into the accounts table, not a separate section")
	}
	if !strings.Contains(page, "Remaining / Total") || !strings.Contains(page, "Usage Rate") {
		t.Fatal("accounts table must carry the quota columns")
	}
}

// TestConsolePageStopsPollingOnAuthFailure verifies that authentication failure stops polling.
//
// CPA tracks management authentication failure counts by client IP, and consecutive failures will ban the IP
// (even correct keys will be rejected), so it must be stopped.
func TestConsolePageStopsPollingOnAuthFailure(t *testing.T) {
	page := Page("/plugins/free-model-hub")
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure to avoid an IP ban")
	}
	if !strings.Contains(page, "401") || !strings.Contains(page, "403") {
		t.Fatal("console page must treat both 401 and 403 as auth failures")
	}
}

// TestConsolePageIsStaticAndSelfContained verifies that the page contains no credential data.
//
// The resource page does not perform management authentication, so the page itself must never carry accounts or tokens.
func TestConsolePageIsStaticAndSelfContained(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, forbidden := range []string{"accessToken", "refreshToken", "device_token", "Authorization: Bearer sk-"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must not embed credential material (%q found)", forbidden)
		}
	}
	// The only network destination must be CPA itself.
	if strings.Contains(page, "http://") && !strings.Contains(page, "http://127.0.0.1") {
		t.Fatal("console page must not reference external origins")
	}
	// The page must be able to stop auto-refresh (to avoid triggering CPA's IP-based authentication ban).
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure")
	}
}

// TestConsolePageWiresLoginCallbackPasteBox verifies that the console wires "Paste Callback Link" to the plugin's
// own management route.
//
// During remote / Docker deployment, the upstream only recognizes localhost callbacks, and the browser cannot reach the plugin's listener; if the page
// does not have this fallback channel, Trae / CodeArts login would stay at "Waiting for authorization…" forever. This locks down
// three things simultaneously: the existence of the fold block, the existence of the submit button, and the request hitting the plugin's own route (not the host route
// —— the host does not have this endpoint, and hitting it will just return 404).
func TestConsolePageWiresLoginCallbackPasteBox(t *testing.T) {
	page := Page("/plugins/free-model-hub")
	for _, marker := range []string{
		`id="loginPasteBox"`,
		`id="loginPasteUrl"`,
		`id="btnSubmitCallback"`,
		`api("POST", "/login/callback"`,
		"paste_callback",
		"callback_hint",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("console page missing %q", marker)
		}
	}
}

// TestConsoleQuotaCellUsesCapability locks down the status determination of the account table quota column.
//
// "Querying..." can only indicate that **a query is actually in progress**. Providers without an upstream quota API (OpenCode ZEN)
// will never have a result; if the page treats "no result" as querying, that row will be permanently stuck in
// "Querying...", while the correct display should be "-- / --". Therefore, the branch must read the capability flag
// supports_quota from the backend, rather than inferring it from whether the result exists.
func TestConsoleQuotaCellUsesCapability(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	if !strings.Contains(page, "account.supports_quota") {
		t.Fatal("quota cell must branch on account.supports_quota")
	}
	// The page must no longer maintain its own quota cache: it is a second copy of the backend cache, and the keys are still guessed.
	if strings.Contains(page, "quotaByAuth") {
		t.Fatal("console must not keep a second quota cache (quotaByAuth); /status is the source of truth")
	}
	// "Querying..." must appear in the branch where supports_quota is true.
	if !regexp.MustCompile(`supports_quota[\s\S]{0,400}Querying`).MatchString(page) {
		t.Fatal("\"Querying...\" must be shown only when the vendor supports quota")
	}
}

// TestConsoleModelChangesTriggerHostReload locks the wiring for "model changes automatically take effect".
//
// The host only rebuilds the model registry during plugin.register / plugin.reconfigure, whereas the plugin's own
// /models/toggle and /models/alias do not notify the host. The host also has no reload endpoint, and the only available
// option is the implicit reload triggered by writing plugin configuration: PATCH /v0/management/plugins/:id/config.
//
// This reload determines whether to execute based on the configuration file hash; unchanged content will be skipped, so it must write a
// changing key (config_reloaded_at). The page must no longer display the prompt bar "Restart CPA to take effect",
// nor provide a manual reload button—changes are synchronized automatically.
func TestConsoleModelChangesTriggerHostReload(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"syncModelConfig",                            // 同步入口
		"config_reloaded_at",                         // 制造配置内容变化的键
		`hostPath("/plugins/free-model-hub/config")`, // 宿主插件配置路由
		`api("PATCH", configPath`,                    // 浅合并写配置，触发宿主重载
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-sync wiring (%s)", literal)
		}
	}

	// Changes must be synchronized successfully rather than left for the user to operate manually.
	for _, hook := range []string{"syncModelConfig();"} {
		if strings.Count(page, hook) < 2 {
			t.Fatalf("console must call %s from both model toggle and alias paths", hook)
		}
	}

	// The old phrasing of "pending activation prompt / restart" must completely disappear.
	for _, stale := range []string{"pendingBar", "setPendingBar", "Restart CPA to take effect", "Apply changes"} {
		if strings.Contains(page, stale) {
			t.Fatalf("console must not keep the restart-pending UI anymore (%s)", stale)
		}
	}
}

// TestConsoleModelLoadModelsButtonRefreshesUpstream locks the true behavior of the "Fetch Model List" button.
//
// Incident: This button was once bound to loadModels (only re-reading the plugin's memory cache). Once the cache is empty (plugin just
// reloaded, probe failed), clicking it forever does nothing, leaving users with no way to recover the model list for an entire provider.
// The button must POST /models/refresh to pull from upstream once, and display upstream errors upon failure—
// 401 and timeout are handled completely differently; hiding them in logs is equivalent to making users guess.
func TestConsoleLoadModelsButtonRefreshesUpstream(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		`api("POST", "/models/refresh"`,     // 真正触发上游拉取
		`refreshModels(false);`,             // 按钮绑定到刷新入口
		"Failed to refresh from upstream: ", // upstream errors shown verbatim
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-refresh wiring (%s)", literal)
		}
	}
	if strings.Contains(page, `addEventListener("click", loadModels`) {
		t.Fatal("the load-models button must not just re-read the local cache")
	}
}

// TestConsoleModelTableMergesIDAndCredits locks the column restructuring of the model table:
// "Model ID" and "Alias" are merged into one column, and the original "Alias" position is changed to "Cost Multiplier".
func TestConsoleModelTableMergesIDAndCredits(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"MODEL_COLUMNS",    // 表头与单元格同源的列描述表
		"modelIDCell",      // 合并列（生效 ID + ✎）
		"modelCreditsCell", // 费用倍率列
		"model.credits",    // 读后端下发的 credits
		`"Model ID"`,
		`"Cost Rate"`,
		`"✎"`,              // 铅笔图标（字面量，不引入 SVG/innerHTML）
		"creditsSortOrder", // 倍率列点击排序
		"modelCreditValue", // 倍率值解析（无倍率沉底）
		"th.sortable",      // 表头可排序样式
		"sort-arrows",      // 双三角排序指示器容器
		"sort-arrow asc",   // 向上三角
		"sort-arrow desc",  // 向下三角
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-table marker %q", literal)
		}
	}

	// The old independent "Alias" header must disappear: it is now part of the Model ID column.
	if strings.Contains(page, `"Name", "Alias"`) {
		t.Fatal("alias column must be merged into the model-id column")
	}

	// Editing still goes through the original interface and synchronization entry point (regression protection).
	for _, literal := range []string{
		`api("POST", "/models/alias"`,
		"syncModelConfig();",
		"scope_id",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("alias editing wiring lost: %s", literal)
		}
	}
}

// TestConsoleModelOptionalColumnsHiddenByDefault locks "Output Limit / Reasoning Tier / Capabilities"
// to be toggleable with default visibility conforming to specifications, and preferences persisted by browser.
func TestConsoleModelOptionalColumnsHiddenByDefault(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		`id="btnModelCols"`,
		`id="modelColsMenu"`,
		"cols-setter",
		`"freehub-model-optional-columns"`,
		"localStorage.getItem(MODEL_COLUMNS_STORE_KEY)",
		"localStorage.setItem(MODEL_COLUMNS_STORE_KEY",
		"function readColumnPrefs()",
		"function visibleColumns()",
		"function toggleOptionalColumn(",
		"Column Display ▾",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing column-visibility wiring %q", literal)
		}
	}

	// There are three toggleable columns: Output Limit, Reasoning Tier, and Capabilities—the remaining columns must always be visible.
	if got := strings.Count(page, "optional: true"); got != 3 {
		t.Fatalf("optional columns = %d, want 3 (output limit/reasoning tier/capabilities)", got)
	}
	for _, key := range []string{"output", "efforts", "caps"} {
		pattern := regexp.MustCompile(`key: "` + key + `"[\s\S]{0,120}optional: true`)
		if !pattern.MatchString(page) {
			t.Fatalf("column %q must be optional", key)
		}
	}

	// Output Limit and Reasoning Tier are hidden by default, while Capabilities are displayed by default.
	for _, key := range []string{"output", "efforts"} {
		pattern := regexp.MustCompile(`key: "` + key + `"[\s\S]{0,120}defaultShow: false`)
		if !pattern.MatchString(page) {
			t.Fatalf("column %q must have defaultShow: false", key)
		}
	}
	if !regexp.MustCompile(`key: "caps"[\s\S]{0,120}defaultShow: true`).MatchString(page) {
		t.Fatal(`column "caps" must have defaultShow: true`)
	}

	// Defaults/corruption/unavailable storage must fall back to an empty object.
	if !regexp.MustCompile(`catch \(e\) \{\s*return \{\};`).MatchString(page) {
		t.Fatal("column preferences must default to an empty object")
	}
}

func TestConsolePageSettingsLabelsAndDisclaimer(t *testing.T) {
	page := Page("/plugins/free-model-hub")
	for _, required := range []string{
		"Compliance and Disclaimer",
		"setKeepaliveHours",
		"MAX_ACCOUNTS_PER_VENDOR = 10",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("console page missing %q", required)
		}
	}
	for _, removed := range []string{"setAutoCheckin", "setAutoTasks", "taskSchedule"} {
		if strings.Contains(page, removed) {
			t.Fatalf("console page must not keep removed setting %q", removed)
		}
	}
}

// TestConsoleSettingsSyncsModelRegistryImmediately locks channel ID saving to take effect immediately:
// When the response contains model_registry_dirty, reuse syncModelConfig to trigger host reload, and the page no longer
// asks the user to "wait for the model list to refresh (or restart the host)".
func TestConsoleSettingsSyncsModelRegistryImmediately(t *testing.T) {
	page := Page("/plugins/free-model-hub")
	if !strings.Contains(page, "if (data && data.model_registry_dirty) { syncModelConfig(); }") {
		t.Fatal("saving settings must call syncModelConfig when model_registry_dirty is set")
	}
	for _, stale := range []string{"wait for the model list to refresh", "automatically add channel tags to"} {
		if strings.Contains(page, stale) {
			t.Fatalf("channel tags apply immediately; the page must not keep the old note: %s", stale)
		}
	}
}

// TestConsoleAccountAliasReusesInlineEditor locks the wiring for account aliases and "only one exists".
//
// Account aliases and model aliases share the same interaction pattern (click ✎, Enter/blur to save, Esc to cancel), and the implementation
// must have only one editableCell: duplicating a second one means keyboard behavior, fallback strategies, and error prompts
// will diverge sooner or later. Account aliases are submitted by account identity file_id, not nickname/email.
func TestConsoleAccountAliasReusesInlineEditor(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"function editableCell(opts)", // 共用编辑器
		"function accountNameCell(account)",
		"accountNameCell(account)", // 账号列确实用它渲染
		`api("POST", "/accounts/alias"`,
		"account.file_id",
		"account.alias",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing account-alias wiring %q", literal)
		}
	}

	if count := strings.Count(page, `input.addEventListener("blur"`); count != 1 {
		t.Fatalf("inline editing must have exactly one implementation, found %d blur handlers", count)
	}
}

// TestConsoleAccountAliasPrefillsCurrentName locks the edit prefill and normalization for account aliases:
// When opening the edit box, prefill the currently active name (consistent with model aliases); leaving it blank or reverting to the original name is regarded as
// "no alias set", and will not leave a duplicate copy in the state with the same name as upstream.
func TestConsoleAccountAliasPrefillsCurrentName(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"function accountOriginalName(account)",
		"prefill: function () { return accountOriginalName(account); }",
		`normalize: function (next) { return next === accountOriginalName(account) ? "" : next; }`,
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing account-alias prefill wiring %q", literal)
		}
	}
}

// TestConsoleAccountsTableSupportsBatchSelection locks the batch operation wiring for the account table:
// Selection column is first, status column is to the left of the provider column, and the operation column has been removed (deletion moved to the top button).
func TestConsoleAccountsTableSupportsBatchSelection(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		`["", "Status", "Provider", "Account", "Plan", "Remaining / Total", "Usage Rate", "Refresh Time"]`,
		"function accountCheckCell(account)",
		`id="btnDeleteAccounts"`,
		`id="btnDisableAccounts"`,
		`id="btnEnableAccounts"`,
		`id="btnSelectAllAccounts"`,
		`id="btnSelectNoneAccounts"`,
		`"/accounts/enabled"`,
		`id="accountFilter"`,
		"function accountMatchesFilter(account, filter)",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing account batch wiring %q", literal)
		}
	}
	if strings.Contains(page, `"Actions"`) {
		t.Fatal("the accounts table must not have an actions column; deletion uses the top button")
	}
}
