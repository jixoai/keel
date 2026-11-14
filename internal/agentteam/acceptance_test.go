// This file is the acceptance matrix of the agent-team wave: it aggregates the
// contract's key invariants into one runnable, self-describing table — one row
// per invariant, one subtest per row, stable row IDs so a failure names exactly
// the clause it broke.
//
// The rows deliberately restate invariants that the unit tests already cover.
// The unit tests guard behavior while the code is written; this matrix states
// what the wave is accepted against:
//
//   - A1..A4  captain exactly one, members 0..N, member uniqueness, the captain
//     is not also a member seat;
//   - A5..A8  avatar sources: same seed is a stable identicon, and an upload or
//     url avatar is never silently replaced by the identicon default;
//   - A9..A12 the model/agent/team CLI trees declare their frozen command sets
//     and one command path never has two owners;
//   - A13..A16 team revision/hash determinism and the team CLI off semantics;
//   - A17..A20 the serving faces that make P7 executable: the capability
//     catalog, the effective captain command face with its typed permission
//     error, the team/agent switch intersection, and the sealed-team start gate.
//
// A5 and A6 differ on purpose: A5 proves the derivation is self-consistent in
// this process, A6 pins it against rows derived independently of this package
// (sha256 over identiconDomain+seed, first three columns per row, mirrored), so
// "same seed, same picture" is not proved by the implementation against itself.
//
// Specs:
//   - openspec/changes/keel-agent-team-v1/specs/agent-team/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/agent-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/plugin-capabilities/spec.md
package agentteam

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// acceptanceRow is one row of the acceptance matrix: a stable ID, the invariant
// it guards, and the check that proves it.
type acceptanceRow struct {
	// ID names the row and becomes the subtest name (A1_captain_exactly_one).
	ID string
	// Invariant is the contract clause the row guards, quoted from the spec.
	Invariant string
	// Check proves the invariant and fails the row when it does not hold.
	Check func(t *testing.T)
}

// TestAcceptanceMatrix runs every row of the acceptance matrix. The runner
// checks the matrix itself first (non-empty, unique IDs), so a row cannot be
// silently dropped or shadowed.
func TestAcceptanceMatrix(t *testing.T) {
	rows := acceptanceMatrix()
	if len(rows) == 0 {
		t.Fatal("the acceptance matrix is empty")
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.Invariant) == "" {
			t.Fatalf("acceptance row %+v must carry both an ID and an invariant", row)
		}
		if seen[row.ID] {
			t.Fatalf("acceptance row ID %q is declared twice", row.ID)
		}
		seen[row.ID] = true
	}
	for _, row := range rows {
		row := row
		t.Run(row.ID, func(t *testing.T) {
			t.Log(row.Invariant)
			row.Check(t)
		})
	}
}

// acceptanceMatrix returns the rows in declaration order.
func acceptanceMatrix() []acceptanceRow {
	return []acceptanceRow{
		{
			ID:        "A1_captain_exactly_one",
			Invariant: "AgentTeam MUST 恰好包含一个 captain：captain 是单个 TeamMember 字段，缺失即 missing_captain 且状态 invalid",
			Check:     checkCaptainExactlyOne,
		},
		{
			ID:        "A2_members_zero_to_N",
			Invariant: "成员 MAY 为 0..N；只有队长的团队是合法团队，0、1、2、5 个成员都必须有效",
			Check:     checkMembersZeroToN,
		},
		{
			ID:        "A3_member_unique",
			Invariant: "同一 Agent MUST NOT 占据两个成员席位（trim 后同 id 也算同一个人）",
			Check:     checkMemberUnique,
		},
		{
			ID:        "A4_captain_not_in_members",
			Invariant: "captain MUST NOT 同时出现在成员列表：captain 席位与成员席位是互斥的",
			Check:     checkCaptainNotInMembers,
		},
		{
			ID:        "A5_avatar_identicon_stable_per_seed",
			Invariant: "identicon MUST 由 seed 本地确定性生成：相同 seed 相同头像（含前后空白），不同 seed 不同头像，改名不改变 seed",
			Check:     checkIdenticonStablePerSeed,
		},
		{
			ID:        "A6_avatar_identicon_golden",
			Invariant: "相同 seed MUST 在任意进程/构建得到相同像素：黄金行由包外独立实现推导，作为跨进程稳定性证据",
			Check:     checkIdenticonGolden,
		},
		{
			ID:        "A7_avatar_upload_not_overwritten",
			Invariant: "upload 头像 MUST 保留 kind=upload 与 blob 引用，MUST NOT 被替换成 identicon；seed 只作加载失败回退 identicon 的确定性种子（默认 agent id）；缺引用则 missing_avatar_ref",
			Check:     checkUploadAvatarNotOverwritten,
		},
		{
			ID:        "A8_avatar_url_not_overwritten",
			Invariant: "url 头像 MUST 保留 kind=url 与 URL，MUST NOT 被替换成 identicon；seed 只作加载失败回退 identicon 的确定性种子（默认 agent id）；只有零值头像才落到 identicon",
			Check:     checkURLAvatarNotOverwritten,
		},
		{
			ID:        "A9_cli_model_tree",
			Invariant: "model CLI 树 MUST 声明 spec 冻结的 provider list|show|add|update|remove|test 与 catalog，且不得多声明或少声明",
			Check:     func(t *testing.T) { checkCLITreeByName(t, "model") },
		},
		{
			ID:        "A10_cli_agent_tree",
			Invariant: "agent CLI 树 MUST 声明 spec 冻结的 list|show|create|update|delete|start|stop，且不得多声明或少声明",
			Check:     func(t *testing.T) { checkCLITreeByName(t, "agent") },
		},
		{
			ID:        "A11_cli_team_tree",
			Invariant: "team CLI 树 MUST 声明 spec 冻结的 list|show|create|update|delete|member add|member remove|save|start，且不得多声明或少声明",
			Check:     func(t *testing.T) { checkCLITreeByName(t, "team") },
		},
		{
			ID:        "A12_cli_path_unique",
			Invariant: "一个命令 path MUST 只有一个 owner：树内重复与跨插件重复都必须被校验拒绝，union 不重复",
			Check:     checkCLIPathUnique,
		},
		{
			ID:        "A13_team_revision_monotonic",
			Invariant: "revision MUST 只经 Seal 单调前移（0 → 1 → 2），与 hash 成对钉住；半写状态 revision_inconsistent。未封存是合法草稿（Validate 通过），但启动门 MUST 要求已封存（见 A20）",
			Check:     checkTeamRevisionMonotonic,
		},
		{
			ID:        "A14_team_hash_deterministic",
			Invariant: "内容 hash MUST 确定且只覆盖内容：集合序无关、重封不变、任一身份/内容字段改动都变、陈旧 hash 即 hash_mismatch",
			Check:     checkTeamHashDeterministic,
		},
		{
			ID:        "A15_teamcli_disabled_fail_closed",
			Invariant: "teamCli 关闭时 MUST 一个子命令都不允许（残留白名单也不行），且 disabled 状态不得携带命令；Agent 面与 Team 面语义一致",
			Check:     checkTeamCLIDisabledFailClosed,
		},
		{
			ID:        "A16_teamcli_enabled_explicit",
			Invariant: "teamCli 开启时 MUST 显式列出白名单：只允许列出的子命令，空白名单 team_cli_no_commands，词表外的名字非法",
			Check:     checkTeamCLIEnabledExplicit,
		},
		{
			ID:        "A17_capability_catalog_served",
			Invariant: "能力树 MUST 有可服务形态：catalog 由三棵 CLI 树构成、可通过自身 JSON 往返、按 install 规则校验通过，且服务集合等于声明集合",
			Check:     checkCapabilityCatalogServed,
		},
		{
			ID:        "A18_captain_face_typed_permission",
			Invariant: "队长命令面 MUST 由能力树 ∩ 有效 teamCli 计算：关闭后命令从面消失，直接调用返回 typed permission error（disabled/not_allowed/not_declared）",
			Check:     checkCaptainFaceTypedPermission,
		},
		{
			ID:        "A19_teamcli_effective_intersection",
			Invariant: "agent 级与 team 级 teamCli 并存时 MUST 有单一裁决：两侧都开才开，白名单取交集，任一侧关闭即关闭",
			Check:     checkTeamCLIEffectiveIntersection,
		},
		{
			ID:        "A20_start_requires_sealed",
			Invariant: "草稿团队（未封存）可 Validate，但启动门 MUST 要求 revision/hash 已封存（not_sealed），否则 run manifest 会冻结空 hash",
			Check:     checkStartRequiresSealed,
		},
	}
}

// acceptanceAgent is a minimal valid agent, local to this file so the matrix
// keeps its own fixture.
func acceptanceAgent() Agent {
	return Agent{ID: "agent_acceptance_lead", Name: "验收主理人"}
}

// acceptanceTeam is the smallest team the spec accepts: one captain, no
// members.
func acceptanceTeam() *AgentTeam {
	return NewTeam("team_acceptance", "验收小队", TeamMember{
		AgentID:     "agent_acceptance_lead",
		Label:       "队长",
		Description: "统筹验收",
	})
}

// acceptanceTeamWithMembers seats one member per agent id.
func acceptanceTeamWithMembers(agentIDs ...string) *AgentTeam {
	team := acceptanceTeam()
	for _, agentID := range agentIDs {
		team.Members = append(team.Members, TeamMember{AgentID: agentID})
	}
	return team
}

// acceptanceMemberIDs returns count distinct member agent ids.
func acceptanceMemberIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("agent_member_%d", index+1)
	}
	return ids
}

// acceptanceCopyTeam deep-copies a team so an edit cannot leak back into the
// fixture through a shared slice.
func acceptanceCopyTeam(team *AgentTeam) *AgentTeam {
	copied := *team
	copied.Members = append([]TeamMember(nil), team.Members...)
	copied.Defaults.Tools = append([]string(nil), team.Defaults.Tools...)
	copied.Capabilities.TeamCLI.Commands = append([]string(nil), team.Capabilities.TeamCLI.Commands...)
	return &copied
}

// A1: the captain is exactly one seat.
func checkCaptainExactlyOne(t *testing.T) {
	t.Helper()

	// Structural half: one field of type TeamMember, not a slice, so the type
	// cannot even express two captains; and no other field can hold a seat.
	captainType := reflect.TypeOf(TeamMember{})
	teamType := reflect.TypeOf(AgentTeam{})
	captainField, ok := teamType.FieldByName("Captain")
	if !ok {
		t.Fatal("AgentTeam.Captain is missing")
	}
	if got, want := captainField.Type, captainType; got != want {
		t.Fatalf("AgentTeam.Captain has type %s, want a single %s (a slice would admit two captains)", got, want)
	}
	memberSlice := reflect.SliceOf(captainType)
	for index := 0; index < teamType.NumField(); index++ {
		field := teamType.Field(index)
		if field.Name == "Captain" || field.Name == "Members" {
			continue
		}
		if field.Type == captainType || field.Type == memberSlice {
			t.Errorf("AgentTeam.%s can hold a member seat; only Captain and Members may", field.Name)
		}
	}

	// Behavioral half: a missing or blank captain is missing_captain + invalid.
	team := acceptanceTeam()
	requireNoError(t, team.Validate())
	if team.Status() != StatusValid {
		t.Fatalf("a captain-only team is %q, want %q", team.Status(), StatusValid)
	}
	requireCode(t, NewTeam("team_blank", "无名队长", TeamMember{}).Validate(), "captain.agentId", CodeMissingCaptain)
	blank := NewTeam("team_blank", "空白队长", TeamMember{AgentID: "   "})
	requireCode(t, blank.Validate(), "captain.agentId", CodeMissingCaptain)
	if blank.Status() != StatusInvalid {
		t.Fatalf("a team without a captain is %q, want %q", blank.Status(), StatusInvalid)
	}
}

// A2: 0..N members are all legal.
func checkMembersZeroToN(t *testing.T) {
	t.Helper()
	for _, count := range []int{0, 1, 2, 5} {
		team := acceptanceTeamWithMembers(acceptanceMemberIDs(count)...)
		if got := len(team.Members); got != count {
			t.Fatalf("seated %d members, want %d", got, count)
		}
		requireNoError(t, team.Validate())
		if team.Status() != StatusValid {
			t.Errorf("%d members: status = %q, want %q", count, team.Status(), StatusValid)
		}
	}

	// An empty but non-nil member list is the same 0-member team, not a defect.
	empty := acceptanceTeam()
	empty.Members = []TeamMember{}
	requireNoError(t, empty.Validate())
}

// A3: the same agent cannot hold two seats.
func checkMemberUnique(t *testing.T) {
	t.Helper()
	team := acceptanceTeamWithMembers("agent_design_lead", "agent_engineer")
	requireNoError(t, team.Validate())

	duplicate := acceptanceTeamWithMembers("agent_design_lead", "agent_engineer", "agent_design_lead")
	requireCode(t, duplicate.Validate(), "members[2].agentId", CodeDuplicateMember)
	if duplicate.Status() != StatusInvalid {
		t.Fatalf("a team with a duplicate member is %q, want %q", duplicate.Status(), StatusInvalid)
	}

	// Trimming means " agent_x " and "agent_x" are the same person.
	padded := acceptanceTeamWithMembers("agent_design_lead", "  agent_design_lead  ")
	requireCode(t, padded.Validate(), "members[1].agentId", CodeDuplicateMember)
}

// A4: the captain is not also a member seat.
func checkCaptainNotInMembers(t *testing.T) {
	t.Helper()
	team := acceptanceTeam()
	seated := acceptanceTeamWithMembers(team.Captain.AgentID)
	requireCode(t, seated.Validate(), "members[0].agentId", CodeCaptainInMembers)
	if seated.Status() != StatusInvalid {
		t.Fatalf("a team seating its captain twice is %q, want %q", seated.Status(), StatusInvalid)
	}

	// The same seat written with padding is still the captain.
	padded := acceptanceTeamWithMembers("  " + team.Captain.AgentID + "  ")
	requireCode(t, padded.Validate(), "members[0].agentId", CodeCaptainInMembers)

	// A distinct member next to the captain stays legal.
	requireNoError(t, acceptanceTeamWithMembers("agent_design_lead").Validate())
}

// A5: same seed, same identicon; different seed, different identicon.
func checkIdenticonStablePerSeed(t *testing.T) {
	t.Helper()
	rows := IdenticonRows("agent_design_lead")
	if len(rows) != IdenticonGridSize {
		t.Fatalf("identicon has %d rows, want %d", len(rows), IdenticonGridSize)
	}
	for row, line := range rows {
		if len(line) != IdenticonGridSize {
			t.Fatalf("row %d = %q, want %d cells", row, line, IdenticonGridSize)
		}
	}
	if again := IdenticonRows("agent_design_lead"); !slices.Equal(again, rows) {
		t.Fatalf("the same seed produced different rows: %v != %v", again, rows)
	}
	// Seed trimming: surrounding space is not part of the identity.
	if padded := IdenticonRows("  agent_design_lead\t"); !slices.Equal(padded, rows) {
		t.Fatalf("trimming changed the identicon: %v != %v", padded, rows)
	}
	if other := IdenticonRows("agent_engineer"); slices.Equal(other, rows) {
		t.Fatalf("two different seeds produced the same identicon: %v", other)
	}

	// The picture follows the seed, not the agent: two agents sharing a seed
	// share the picture, and renaming keeps the seed. (A5 stays a pure
	// derivation check; the edit path is covered by A20's normalization rule.)
	one := Agent{ID: "agent_one", Name: "第一个名字", Avatar: Avatar{Kind: AvatarIdenticon, Seed: "shared-seed"}}.Normalized()
	two := Agent{ID: "agent_two", Name: "另一个名字", Avatar: Avatar{Kind: AvatarIdenticon, Seed: "shared-seed"}}.Normalized()
	if !slices.Equal(IdenticonRows(one.Avatar.SeedOr(one.ID)), IdenticonRows(two.Avatar.SeedOr(two.ID))) {
		t.Fatal("the same avatar seed produced different identicons")
	}
	renamed := one
	renamed.Name = "改过的名字"
	if got := renamed.Normalized().Avatar.Seed; got != "shared-seed" {
		t.Fatalf("renaming changed the avatar seed to %q", got)
	}
}

// checkIdenticonGolden pins two seeds to rows derived outside this package, so
// a change to the derivation (or to a dependency it quietly starts using) is a
// visible acceptance failure instead of a compatible-looking new picture.
func checkIdenticonGolden(t *testing.T) {
	t.Helper()
	golden := []struct {
		seed string
		rows []string
	}{
		{
			seed: "agent_design_lead",
			rows: []string{"#####", ".#.#.", "##.##", "..#..", "....."},
		},
		{
			seed: "agent_engineer",
			rows: []string{".....", "##.##", ".....", "##.##", "#.#.#"},
		},
	}
	for _, want := range golden {
		if got := IdenticonRows(want.seed); !slices.Equal(got, want.rows) {
			t.Errorf("identicon(%q) = %v, want the independently derived rows %v", want.seed, got, want.rows)
		}
	}
}

// A7: an uploaded avatar keeps its kind and blob; the default only fills in the
// deterministic fallback seed used when the upload cannot be loaded.
func checkUploadAvatarNotOverwritten(t *testing.T) {
	t.Helper()
	upload := Avatar{Kind: AvatarUpload, Ref: "blob:sha256:4f9c"}
	normalized := upload.WithDefaults("agent_design_lead")
	if normalized.Kind != AvatarUpload {
		t.Fatalf("upload avatar kind = %q, want %q", normalized.Kind, AvatarUpload)
	}
	if normalized.Ref != "blob:sha256:4f9c" {
		t.Fatalf("upload avatar ref = %q, want the stored blob reference", normalized.Ref)
	}
	if normalized.Seed != "agent_design_lead" {
		t.Fatalf("upload avatar fallback seed = %q, want the agent id so a failed load can fall back to an identicon", normalized.Seed)
	}
	requireNoError(t, normalized.Validate())

	// An explicit seed is the caller's decision and is kept as written.
	explicit := Avatar{Kind: AvatarUpload, Ref: upload.Ref, Seed: "custom-seed"}
	if got := explicit.WithDefaults("agent_design_lead").Seed; got != "custom-seed" {
		t.Fatalf("upload avatar seed = %q, want the caller's seed", got)
	}

	// The agent-level normalization runs the same default and keeps the source.
	agent := acceptanceAgent()
	agent.Avatar = upload
	requireNoError(t, agent.Validate())
	got := agent.Normalized().Avatar
	if got.Kind != AvatarUpload || got.Ref != upload.Ref {
		t.Fatalf("agent normalization rewrote the upload avatar: %+v", got)
	}
	if got.Seed != agent.ID {
		t.Fatalf("normalized upload avatar seed = %q, want the agent id %q", got.Seed, agent.ID)
	}

	// An upload without a stored reference is rejected instead of falling back
	// to an identicon, so a broken upload is visible rather than silent.
	requireCode(t, Avatar{Kind: AvatarUpload}.Validate(), "avatar.ref", CodeMissingAvatarRef)
}

// A8: a URL avatar keeps its kind and URL; the default only fills in the
// deterministic fallback seed.
func checkURLAvatarNotOverwritten(t *testing.T) {
	t.Helper()
	source := "https://cdn.example.com/avatar.png"
	urlAvatar := Avatar{Kind: AvatarURL, URL: source}
	normalized := urlAvatar.WithDefaults("agent_design_lead")
	if normalized.Kind != AvatarURL {
		t.Fatalf("url avatar kind = %q, want %q", normalized.Kind, AvatarURL)
	}
	if normalized.URL != source {
		t.Fatalf("url avatar url = %q, want %q", normalized.URL, source)
	}
	if normalized.Seed != "agent_design_lead" {
		t.Fatalf("url avatar fallback seed = %q, want the agent id so a failed load can fall back to an identicon", normalized.Seed)
	}
	requireNoError(t, normalized.Validate())

	agent := acceptanceAgent()
	agent.Avatar = urlAvatar
	requireNoError(t, agent.Validate())
	got := agent.Normalized().Avatar
	if got.Kind != AvatarURL || got.URL != source {
		t.Fatalf("agent normalization rewrote the url avatar: %+v", got)
	}

	// Only the zero value falls back to the identicon, and then it is seeded
	// with the agent id — that is the single place the default kind applies.
	zero := Avatar{}.WithDefaults("agent_design_lead")
	if zero.Kind != AvatarIdenticon || zero.Seed != "agent_design_lead" {
		t.Fatalf("zero avatar normalized to %+v, want an identicon seeded with the agent id", zero)
	}
}

// A9..A11: one plugin's frozen command set is declared exactly once and the
// tree validates as a manifest. The expectation is written out here, not
// derived from the tree, so a dropped command cannot shrink both sides.
func checkCLITreeByName(t *testing.T, name string) {
	t.Helper()
	for _, expectation := range acceptanceCLIExpectations() {
		if expectation.name != name {
			continue
		}
		checkCLITree(t, expectation.spec(), expectation)
		return
	}
	t.Fatalf("no acceptance expectation is named %q", name)
}

// acceptanceCLIExpectation is one plugin's frozen command set.
type acceptanceCLIExpectation struct {
	// name is the tree's top-level CLI segment ("model", "agent", "team").
	name string
	// pluginID is the manifest id that owns the tree.
	pluginID string
	// spec builds the tree under test.
	spec func() PluginCLISpec
	// want is the command set the plugin's spec freezes.
	want [][]string
}

// acceptanceCLIExpectations states the three frozen command sets.
func acceptanceCLIExpectations() []acceptanceCLIExpectation {
	return []acceptanceCLIExpectation{
		{
			name:     "model",
			pluginID: "keel.model",
			spec:     ModelPluginCLISpec,
			want: [][]string{
				{"model", "provider", "list"},
				{"model", "provider", "show"},
				{"model", "provider", "add"},
				{"model", "provider", "update"},
				{"model", "provider", "remove"},
				{"model", "provider", "test"},
				{"model", "catalog"},
			},
		},
		{
			name:     "agent",
			pluginID: "keel.agent",
			spec:     AgentPluginCLISpec,
			want: [][]string{
				{"agent", "list"},
				{"agent", "show"},
				{"agent", "create"},
				{"agent", "update"},
				{"agent", "delete"},
				{"agent", "start"},
				{"agent", "stop"},
			},
		},
		{
			name:     "team",
			pluginID: "keel.agent-team",
			spec:     TeamPluginCLISpec,
			want: [][]string{
				{"team", "list"},
				{"team", "show"},
				{"team", "create"},
				{"team", "update"},
				{"team", "delete"},
				{"team", "member", "add"},
				{"team", "member", "remove"},
				{"team", "member", "list"},
				{"team", "member", "save"},
				{"team", "save"},
				{"team", "start"},
			},
		},
	}
}

// checkCLITree proves one tree declares exactly its frozen command set.
func checkCLITree(t *testing.T, spec PluginCLISpec, want acceptanceCLIExpectation) {
	t.Helper()
	if spec.PluginID != want.pluginID {
		t.Fatalf("spec id = %q, want %q", spec.PluginID, want.pluginID)
	}
	declared := pathSet(spec.Capabilities)
	if got := len(spec.Capabilities); got != len(want.want) {
		t.Errorf("%s declares %d commands, want %d: %v", want.pluginID, got, len(want.want), pathKeys(spec.Capabilities))
	}
	for _, path := range want.want {
		key := strings.Join(path, " ")
		if !declared[key] {
			t.Errorf("%s does not declare %q", want.pluginID, "keel "+key)
		}
		if !containsPath(spec.Required, path) {
			t.Errorf("%s Required does not freeze %q", want.pluginID, "keel "+key)
		}
	}
	if got := len(spec.Required); got != len(want.want) {
		t.Errorf("%s freezes %d required paths, want %d", want.pluginID, got, len(want.want))
	}
	for _, path := range spec.Required {
		if !declared[strings.Join(path, " ")] {
			t.Errorf("%s requires %q but does not declare it", want.pluginID, commandHead(path))
		}
	}
	for _, capability := range spec.Capabilities {
		if len(capability.Path) < 2 || capability.Path[0] != want.name {
			t.Errorf("%s is not a %q command", capability.Command(), want.name)
		}
	}

	// The tree is only accepted if the host's own validator accepts it: the
	// manifest is what --help, the settings page and dispatch read.
	requireNoError(t, spec.Validate())
	requireNoError(t, spec.Manifest().Validate(pluginpkg.Environment{KeelVersion: "0.2.0"}))
}

// A12: one command path, one owner.
func checkCLIPathUnique(t *testing.T) {
	t.Helper()
	owner := make(map[string]string, 32)
	for _, spec := range PluginCLISpecs() {
		for _, capability := range spec.Capabilities {
			key := capability.Key()
			if previous, exists := owner[key]; exists {
				t.Errorf("path %q is owned by %s and %s", capability.Command(), previous, spec.PluginID)
				continue
			}
			owner[key] = spec.PluginID
		}
	}

	union := CLICapabilities()
	if len(union) != len(owner) {
		t.Fatalf("the union exposes %d commands, the trees declare %d distinct paths", len(union), len(owner))
	}
	seen := make(map[string]bool, len(union))
	for _, capability := range union {
		key := capability.Key()
		if seen[key] {
			t.Fatalf("the union repeats %q", capability.Command())
		}
		seen[key] = true
		if got, want := capability.Command(), "keel "+key; got != want {
			t.Errorf("command head %q does not name path %q", got, key)
		}
	}

	// Uniqueness is enforced, not merely true of today's data: a tree that
	// declares one path twice fails its own manifest validation.
	duplicated := AgentPluginCLISpec()
	declared := duplicated.CLI()
	duplicated.Capabilities = append(declared, declared[0])
	if err := duplicated.Validate(); err == nil {
		t.Fatal("a tree declaring one path twice validated")
	}

	// A path cannot move between plugins: the three trees share one namespace,
	// so one command may not have two owners (two danger classes).
	model := ModelPluginCLISpec()
	foreign, ok := findCapability(model.Capabilities, "model", "catalog")
	if !ok {
		t.Fatal("keel model catalog is missing")
	}
	agent := AgentPluginCLISpec()
	agent.Capabilities = append(agent.CLI(), foreign)
	if err := ValidatePluginCLISpecs([]PluginCLISpec{model, agent}); err == nil {
		t.Fatal("two plugins owning one command path passed the cross-plugin check")
	}
	requireNoError(t, ValidatePluginCLISpecs(PluginCLISpecs()))
}

// A13: the revision only moves forward through Seal, paired with the hash.
func checkTeamRevisionMonotonic(t *testing.T) {
	t.Helper()
	team := acceptanceTeam()
	if team.Revision() != 0 || team.Sealed() {
		t.Fatalf("a new team must be unsealed, got revision %d sealed=%v", team.Revision(), team.Sealed())
	}
	// An unsealed team is still a legal configuration: sealing is what a write
	// path does before persisting, not a validity precondition.
	requireNoError(t, team.Validate())

	content := team.Hash()
	team.Seal()
	if team.Revision() != 1 || !team.Sealed() {
		t.Fatalf("after the first seal: revision %d sealed=%v, want 1/true", team.Revision(), team.Sealed())
	}
	team.Seal()
	if team.Revision() != 2 {
		t.Fatalf("after the second seal: revision %d, want 2", team.Revision())
	}
	if team.Hash() != content {
		t.Fatalf("re-sealing unchanged content changed the hash: %s != %s", team.Hash(), content)
	}

	// The stamp travels with the config and survives a round trip.
	blob, err := json.Marshal(team)
	requireNoError(t, err)
	var wire map[string]any
	requireNoError(t, json.Unmarshal(blob, &wire))
	if _, ok := wire["hash"]; !ok {
		t.Errorf("the encoded team does not carry the hash: %s", blob)
	}
	if wire["revision"] != float64(2) {
		t.Errorf("encoded revision = %v, want 2", wire["revision"])
	}
	var decoded AgentTeam
	requireNoError(t, json.Unmarshal(blob, &decoded))
	if decoded.Revision() != 2 || decoded.Hash() != content || !decoded.Sealed() {
		t.Fatalf("round trip lost the stamp: revision %d sealed=%v hash %s", decoded.Revision(), decoded.Sealed(), decoded.Hash())
	}
	requireNoError(t, decoded.Validate())

	// Half a stamp is inconsistent: a caller cannot bump the revision alone.
	revisionOnly := *team
	revisionOnly.storedHash = ""
	requireCode(t, revisionOnly.Validate(), "revision", CodeRevisionInconsistent)
	hashOnly := *team
	hashOnly.revision = 0
	requireCode(t, hashOnly.Validate(), "revision", CodeRevisionInconsistent)

	// And neither half is reachable from outside: no exported field carries it.
	teamType := reflect.TypeOf(AgentTeam{})
	for index := 0; index < teamType.NumField(); index++ {
		field := teamType.Field(index)
		switch field.Name {
		case "revision", "storedHash":
			if field.IsExported() {
				t.Errorf("AgentTeam.%s is exported; the seal must not be caller writable", field.Name)
			}
		}
	}
}

// A14: the content hash is deterministic and covers every content field.
func checkTeamHashDeterministic(t *testing.T) {
	t.Helper()
	one := acceptanceTeamWithMembers("agent_design_lead", "agent_engineer")
	one.Defaults.Tools = []string{"write", "bash", "read"}
	one.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLISave, TeamCLIMemberAdd}}

	// The same content listed in another order is the same configuration.
	two := acceptanceTeamWithMembers("agent_design_lead", "agent_engineer")
	two.Defaults.Tools = []string{"bash", "read", "write"}
	two.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLISave}}
	if one.Hash() == "" {
		t.Fatal("a valid team must have a content hash")
	}
	if one.Hash() != two.Hash() {
		t.Fatalf("set-like ordering changed the hash: %s != %s", one.Hash(), two.Hash())
	}

	// The digest is a sha256 hex string, stable across encode/decode.
	digest := one.Hash()
	if len(digest) != 64 {
		t.Fatalf("hash = %q, want 64 hex characters", digest)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		t.Fatalf("hash %q is not hex: %v", digest, err)
	}
	blob, err := json.Marshal(one)
	requireNoError(t, err)
	var decoded AgentTeam
	requireNoError(t, json.Unmarshal(blob, &decoded))
	if decoded.Hash() != digest {
		t.Fatalf("round trip changed the hash: %s != %s", decoded.Hash(), digest)
	}

	// Every content field is covered: an edit that bypasses Seal changes the
	// hash, including the team-local member label.
	edits := []struct {
		name string
		edit func(*AgentTeam)
	}{
		{"team name", func(edited *AgentTeam) { edited.Name = "改名后的团队" }},
		{"team description", func(edited *AgentTeam) { edited.Description = "新的描述" }},
		{"captain seat", func(edited *AgentTeam) { edited.Captain.AgentID = "agent_other_captain" }},
		{"member label", func(edited *AgentTeam) { edited.Members[0].Label = "新标签" }},
		{"default tools", func(edited *AgentTeam) { edited.Defaults.Tools[0] = "grep" }},
		{"team cli allowlist", func(edited *AgentTeam) {
			edited.Capabilities.TeamCLI.Commands = []string{TeamCLIMemberAdd}
		}},
	}
	for _, edit := range edits {
		edit := edit
		t.Run(edit.name, func(t *testing.T) {
			edited := acceptanceCopyTeam(one)
			edit.edit(edited)
			if edited.Hash() == digest {
				t.Fatalf("editing %s did not change the content hash %s", edit.name, digest)
			}
		})
	}

	// A stale seal is rejected instead of trusted: an edit after Seal leaves a
	// hash that no longer matches the content.
	sealed := acceptanceCopyTeam(one)
	sealed.Seal()
	requireNoError(t, sealed.Validate())
	sealed.Name = "封存后又改名"
	requireCode(t, sealed.Validate(), "hash", CodeHashMismatch)
	if sealed.Status() != StatusInvalid {
		t.Fatalf("a stale seal reports %q, want %q", sealed.Status(), StatusInvalid)
	}
}

// acceptanceValidateAgentTeamCLI validates a team CLI switch through the agent
// face (Agent.TeamCLI).
func acceptanceValidateAgentTeamCLI(cli TeamCLI) error {
	agent := acceptanceAgent()
	agent.TeamCLI = cli
	return agent.Validate()
}

// acceptanceValidateTeamTeamCLI validates a team CLI switch through the team
// face (AgentTeam.Capabilities.TeamCLI).
func acceptanceValidateTeamTeamCLI(cli TeamCLI) error {
	team := acceptanceTeam()
	team.Capabilities.TeamCLI = cli
	return team.Validate()
}

// A15: an off switch allows nothing, on either face.
func checkTeamCLIDisabledFailClosed(t *testing.T) {
	t.Helper()
	faces := []struct {
		name     string
		field    string
		validate func(TeamCLI) error
	}{
		{"agent_face", "teamCli.commands", acceptanceValidateAgentTeamCLI},
		{"team_face", "capabilities.teamCli.commands", acceptanceValidateTeamTeamCLI},
	}
	known := KnownTeamSubcommands()
	for _, face := range faces {
		face := face
		t.Run(face.name, func(t *testing.T) {
			// Off with an empty list is the legal off state.
			disabled := TeamCLI{}
			requireNoError(t, face.validate(disabled))
			for _, subcommand := range known {
				if disabled.Allows(subcommand) {
					t.Errorf("a disabled team CLI allows %q", subcommand)
				}
			}

			// Off with a leftover allowlist is rejected on save, and even in
			// that invalid state it still allows nothing: a closed switch
			// cannot be reopened by stale data.
			stale := TeamCLI{Enabled: false, Commands: known}
			requireCode(t, face.validate(stale), face.field, CodeTeamCLICommandsWhenDisabled)
			for _, subcommand := range known {
				if stale.Allows(subcommand) {
					t.Errorf("a disabled team CLI with leftovers allows %q", subcommand)
				}
			}
		})
	}
}

// A16: an on switch allows exactly its explicit allowlist and nothing else.
func checkTeamCLIEnabledExplicit(t *testing.T) {
	t.Helper()
	allowed := []string{TeamCLISave, TeamCLIMemberAdd}
	cli := TeamCLI{Enabled: true, Commands: allowed}
	requireNoError(t, acceptanceValidateAgentTeamCLI(cli))
	requireNoError(t, acceptanceValidateTeamTeamCLI(cli))

	for _, subcommand := range KnownTeamSubcommands() {
		want := slices.Contains(allowed, subcommand)
		if got := cli.Allows(subcommand); got != want {
			t.Errorf("Allows(%q) = %v, want %v", subcommand, got, want)
		}
	}

	// Enabled with no allowlist fails closed: there is no "enable everything".
	open := TeamCLI{Enabled: true}
	requireCode(t, acceptanceValidateAgentTeamCLI(open), "teamCli.commands", CodeTeamCLINoCommands)
	requireCode(t, acceptanceValidateTeamTeamCLI(open), "capabilities.teamCli.commands", CodeTeamCLINoCommands)
	for _, subcommand := range KnownTeamSubcommands() {
		if open.Allows(subcommand) {
			t.Errorf("an open team CLI allows %q; the allowlist must be explicit", subcommand)
		}
	}

	// The allowlist vocabulary is closed: unknown names and blank entries are
	// rejected instead of being carried as dead permissions.
	for _, bad := range []string{"member.promote", "   ", "team.delete"} {
		unknown := TeamCLI{Enabled: true, Commands: []string{bad}}
		requireCode(t, acceptanceValidateAgentTeamCLI(unknown), "teamCli.commands[0]", CodeInvalidTeamCLICommand)
	}

	// Padding inside an allowlist entry is not a different subcommand.
	padded := TeamCLI{Enabled: true, Commands: []string{"  " + TeamCLISave + "  "}}
	requireNoError(t, acceptanceValidateAgentTeamCLI(padded))
	if !padded.Allows(TeamCLISave) {
		t.Fatal("a padded allowlist entry no longer allows its subcommand")
	}

	// The vocabulary itself is a stable, sorted set, and every team CLI tree
	// command names one of its subcommands (no tree command outside the
	// allowlist vocabulary).
	known := KnownTeamSubcommands()
	if !slices.IsSorted(known) {
		t.Errorf("KnownTeamSubcommands() is not sorted: %v", known)
	}
	seen := make(map[string]bool, len(known))
	for _, subcommand := range known {
		if seen[subcommand] {
			t.Errorf("KnownTeamSubcommands() repeats %q", subcommand)
		}
		seen[subcommand] = true
	}
	declared := make(map[string]bool, len(known))
	for _, capability := range TeamPluginCLISpec().Capabilities {
		subcommand, ok := TeamSubcommand(capability.Path)
		if !ok {
			t.Fatalf("%s: team command must start with the team segment", capability.Command())
		}
		if !seen[subcommand] {
			t.Errorf("%s is not a known team subcommand %v", capability.Command(), known)
		}
		declared[subcommand] = true
	}
	// Bidirectional coverage: an allowlist entry without a declared command is a
	// granted dead command, so the vocabulary may not exceed the tree either.
	for _, subcommand := range known {
		if !declared[subcommand] {
			t.Errorf("TeamCLI subcommand %q has no declared command; allowing it would grant a dead command", subcommand)
		}
	}
}

// A17: the capability tree has a serving form, not only a declaration.
func checkCapabilityCatalogServed(t *testing.T) {
	t.Helper()
	catalog := ServeCapabilityCatalog()
	if got := len(catalog.Plugins); got != 3 {
		t.Fatalf("catalog carries %d plugins, want 3", got)
	}
	requireNoError(t, catalog.Validate())

	blob, err := catalog.JSON()
	requireNoError(t, err)
	var decoded CapabilityCatalog
	requireNoError(t, json.Unmarshal(blob, &decoded))
	if len(decoded.Plugins) != len(catalog.Plugins) {
		t.Fatalf("catalog round trip changed the plugin count")
	}

	served := make(map[string]bool, 32)
	for _, plugin := range decoded.Plugins {
		if plugin.PluginID == "" || plugin.Version == "" {
			t.Errorf("served plugin %+v has no identity", plugin)
		}
		for _, capability := range plugin.Commands {
			if served[capability.Key()] {
				t.Errorf("catalog serves %q twice", capability.Command())
			}
			served[capability.Key()] = true
		}
	}
	declared := pathSet(CLICapabilities())
	if len(served) != len(declared) {
		t.Fatalf("catalog serves %d commands, the trees declare %d", len(served), len(declared))
	}
	for key := range declared {
		if !served[key] {
			t.Errorf("catalog is missing %q", "keel "+key)
		}
	}
}

// A18: the captain face is computed, and a direct call is refused with a typed
// error whose reason distinguishes the three cases.
func checkCaptainFaceTypedPermission(t *testing.T) {
	t.Helper()
	spec := TeamPluginCLISpec()
	on := TeamCLI{Enabled: true, Commands: []string{
		TeamCLIMemberAdd, TeamCLIMemberRemove, TeamCLIMemberList,
		TeamCLIMemberSave, TeamCLISave, TeamCLIStart,
	}}
	face := pathSet(CaptainTeamCapabilities(spec, on, on))
	for _, key := range []string{
		"team member add", "team member remove", "team member list",
		"team member save", "team save", "team start",
	} {
		if !face[key] {
			t.Errorf("the captain face is missing %q", "keel "+key)
		}
	}
	requireNoError(t, AuthorizeCaptainCommand(spec, on, on, "team", "member", "add"))

	if face := CaptainTeamCapabilities(spec, TeamCLI{}, on); len(face) != 0 {
		t.Fatalf("a disabled switch still exposes %v", pathKeys(face))
	}
	var permission *PermissionError
	if err := AuthorizeCaptainCommand(spec, TeamCLI{}, on, "team", "member", "add"); !errors.As(err, &permission) || permission.Code() != "team_cli_disabled" {
		t.Fatalf("disabled call error = %v, want a typed team_cli_disabled", err)
	}
	narrow := TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd}}
	if err := AuthorizeCaptainCommand(spec, narrow, on, "team", "member", "remove"); !errors.As(err, &permission) || permission.Code() != "team_cli_not_allowed" {
		t.Fatalf("non-allowlisted call error = %v, want a typed team_cli_not_allowed", err)
	}
	if err := AuthorizeCaptainCommand(spec, on, on, "team", "delete"); !errors.As(err, &permission) || permission.Code() != "team_cli_not_declared" {
		t.Fatalf("undeclared call error = %v, want a typed team_cli_not_declared", err)
	}
}

// A19: the two switches have one ruling, and an agent list cannot widen the
// team list.
func checkTeamCLIEffectiveIntersection(t *testing.T) {
	t.Helper()
	team := TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLIMemberRemove, TeamCLISave}}
	agent := TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLIMemberRemove}}
	effective := EffectiveTeamCLI(team, agent)
	if !effective.Enabled {
		t.Fatal("both switches are on, the effective surface must be on")
	}
	if !effective.Allows(TeamCLIMemberAdd) || !effective.Allows(TeamCLIMemberRemove) {
		t.Errorf("effective allowlist = %v, want the intersection", effective.Commands)
	}
	if effective.Allows(TeamCLISave) {
		t.Error("the agent list must not widen the team list")
	}

	// Either side off closes the surface, whatever the other says.
	for _, closed := range []struct {
		name  string
		team  TeamCLI
		agent TeamCLI
	}{
		{"team off", TeamCLI{Commands: team.Commands}, agent},
		{"agent off", team, TeamCLI{Commands: agent.Commands}},
	} {
		closed := closed
		t.Run(closed.name, func(t *testing.T) {
			effective := EffectiveTeamCLI(closed.team, closed.agent)
			if effective.Enabled {
				t.Fatalf("effective surface is enabled with %s", closed.name)
			}
			for _, subcommand := range KnownTeamSubcommands() {
				if effective.Allows(subcommand) {
					t.Errorf("%s allows %q", closed.name, subcommand)
				}
			}
		})
	}

	// A stored-but-invalid allowlist cannot widen anything either: Allows reads
	// the enabled bit first.
	stale := TeamCLI{Enabled: false, Commands: team.Commands}
	if stale.Allows(TeamCLIMemberAdd) {
		t.Fatal("a disabled switch with leftovers allows a command")
	}
}

// A20: a draft team validates, but the start gate requires a seal.
func checkStartRequiresSealed(t *testing.T) {
	t.Helper()
	team := acceptanceTeam()
	team.Members = []TeamMember{{AgentID: "agent_design_lead"}}
	requireNoError(t, team.Validate())
	requireCode(t, team.ValidateForStart(nil), "revision", CodeNotSealed)
	if team.Status() != StatusValid {
		t.Fatalf("an unsealed draft is %q, want %q (sealing is a start gate, not a validity rule)", team.Status(), StatusValid)
	}

	team.Seal()
	requireNoError(t, team.ValidateForStart(nil))
	requireNoError(t, team.Validate())

	// The start gate also refuses a missing agent and an archived team, so a run
	// cannot start from a config the settings page would show as invalid.
	exists := func(agentID string) bool { return agentID == "agent_acceptance_lead" }
	requireCode(t, team.ValidateForStart(exists), "members[0].agentId", CodeAgentNotFound)

	archived := acceptanceCopyTeam(team)
	archived.Archived = true
	requireCode(t, archived.ValidateForStart(nil), "archived", CodeTeamArchived)

	// Normalization is the single hash input: an inline agent hashes the same
	// whether or not the caller normalized it before sealing.
	one := acceptanceTeam()
	one.Members = []TeamMember{{
		AgentID: "agent_inline",
		Inline:  &Agent{ID: "agent_inline", Name: "内联"},
	}}
	two := acceptanceTeam()
	two.Members = []TeamMember{{
		AgentID: "agent_inline",
		Inline:  &Agent{ID: "agent_inline", Name: "内联", Avatar: Avatar{Kind: AvatarIdenticon, Seed: "agent_inline"}},
	}}
	if one.Hash() != two.Hash() {
		t.Fatalf("normalization changed the hash: %s != %s", one.Hash(), two.Hash())
	}
	one.Seal()
	requireNoError(t, one.Validate())
}
