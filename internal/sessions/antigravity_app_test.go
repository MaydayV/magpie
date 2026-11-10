package sessions

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// antigravitySchema is a conversation database's tables as Antigravity
// 2.23.0 makes them (yygutn's `.schema` on #2190).
const antigravitySchema = "CREATE TABLE `trajectory_meta` (`trajectory_id` text,`cascade_id` text,`trajectory_type` integer,`source` integer,PRIMARY KEY (`trajectory_id`));\n" +
	"CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`has_subtrajectory` numeric NOT NULL DEFAULT false,`metadata` blob,`error_details` blob,`permissions` blob,`task_details` blob,`render_info` blob,`step_payload` blob,`step_format` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));\n" +
	"CREATE INDEX `idx_steps_status` ON `steps`(`status`);\n" +
	"CREATE INDEX `idx_steps_step_type` ON `steps`(`step_type`);\n" +
	"CREATE TABLE `gen_metadata` (`idx` integer,`data` blob,`size` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));\n" +
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));\n" +
	"CREATE TABLE `parent_references` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));\n" +
	"CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT \"main\",`data` blob,PRIMARY KEY (`id`));\n" +
	"CREATE TABLE `battle_mode_infos` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));"

// antigravityModelField is gen_metadata.data's field 19 as yygutn quoted
// it on #2190, the same for generators 0, 1 and 2: "gemini-3.8-flash-n".
// The rest of the row is the request's text, which wasn't posted, so here
// the field is the whole of the message in field 1 it sits in.
const antigravityModelField = "9A011267656D696E692D332E382D666C6173682D6E"

// antigravityFixture makes a conversation database in the app's data
// folder under home, in WAL mode as the app keeps it, with the four step
// rows yygutn posted on #2190 (testdata/antigravity/steps.tsv, the bytes
// as posted), a step with no metadata, a user's step whose metadata has
// no model call, and generators 0 to 2. Step 11's generator (5) has no
// row.
func antigravityFixture(t *testing.T, home, id string) string {
	t.Helper()
	dir := filepath.Join(home, ".gemini", "antigravity", "conversations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(antigravitySchema); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/antigravity/steps.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		idx, h, _ := strings.Cut(line, "\t")
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO steps (idx, step_type, metadata) VALUES (?, 15, ?)", idx, b); err != nil {
			t.Fatal(err)
		}
	}
	// a step with no metadata, and one whose metadata is only its time
	// (the first 13 bytes of step 1's: field 1)
	user, _ := hex.DecodeString("0A0B08DEBCB6D60610B8AAB35C")
	if _, err := db.Exec("INSERT INTO steps (idx, step_type) VALUES (2, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO steps (idx, step_type, metadata) VALUES (0, 14, ?)", user); err != nil {
		t.Fatal(err)
	}
	field, _ := hex.DecodeString(antigravityModelField)
	gen := append([]byte{0x0A, byte(len(field))}, field...)
	for i := range 3 {
		if _, err := db.Exec("INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, ?)", i, gen, len(gen)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// #2190 (yygutn), xxooioo on Discord: the Antigravity app's own calls,
// made without magpie, reach the usage ledger and the Sessions page,
// with the tokens yygutn's figures for those rows give.
func TestAntigravityAppCalls(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	const id = "37c3c786-9a11-44be-97df-b8fc1ccebc78"
	antigravityFixture(t, home, id)

	var got []Call
	for _, c := range Calls(time.Time{}) {
		if c.Agent == "antigravity" {
			got = append(got, c)
		}
	}
	slices.SortFunc(got, func(a, b Call) int { return a.Time.Compare(b.Time) })
	want := []struct {
		sec    int64
		nanos  int64
		model  string
		tokens Tokens
	}{
		{1791860318, 193779000, "gemini-3.8-flash", Tokens{Input: 13317, Output: 621}},
		{1791860328, 770822000, "gemini-3.8-flash", Tokens{Input: 14184, Output: 164}},
		{1791860333, 626675000, "gemini-3.8-flash", Tokens{Input: 15531, Output: 194}},
		{1791860353, 992204000, "", Tokens{Input: 4254, Output: 276, CacheRead: 18561}},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d calls (none from a step without a model call), got %d: %+v", len(want), len(got), got)
	}
	msgs := map[string]bool{}
	for i, w := range want {
		c := got[i]
		at := time.Unix(w.sec, w.nanos)
		if !c.Time.Equal(at) || c.Session != id || c.Tokens != w.tokens || c.Model != w.model {
			t.Errorf("call %d: %v %s %q %+v, want %v %s %q %+v", i, c.Time, c.Session, c.Model, c.Tokens, at, id, w.model, w.tokens)
		}
		if c.Msg == "" || msgs[c.Msg] {
			t.Errorf("call %d: step id %q to count it once by is empty or taken", i, c.Msg)
		}
		msgs[c.Msg] = true
	}

	list := List(0)
	i := slices.IndexFunc(list, func(s Session) bool { return s.Agent == "antigravity" })
	if i < 0 {
		t.Fatalf("no Antigravity session in %+v", list)
	}
	s := list[i]
	if s.ID != id || !s.ReadOnly {
		t.Errorf("session %s read only %v, want %s read only", s.ID, s.ReadOnly, id)
	}
	var sum Tokens
	for _, m := range s.Models {
		sum.add(m.Tokens)
	}
	if want := (Tokens{Input: 13317 + 14184 + 15531 + 4254, Output: 621 + 164 + 194 + 276, CacheRead: 18561}); sum != want {
		t.Errorf("session tokens %+v, want %+v (models %+v)", sum, want, s.Models)
	}
}

// A response id two steps share is one call, counted at its last step.
func TestAntigravityAppOneResponseOnce(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	path := antigravityFixture(t, home, "b9eace9a-b15a-4dde-a440-b52e02016fa5")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var b []byte
	if err := db.QueryRow("SELECT metadata FROM steps WHERE idx = 1").Scan(&b); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO steps (idx, metadata) VALUES (4, ?)", b); err != nil {
		t.Fatal(err)
	}
	calls, err := antigravityRead(path)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, c := range calls {
		steps = append(steps, strconv.FormatInt(c.step, 10))
	}
	if got := strings.Join(steps, ","); got != "4,3,5,11" {
		t.Errorf("calls at steps %s, want 4,3,5,11 (step 1's response once, at step 4)", got)
	}
}

// A metadata row cut short is no call, and the rest are still read.
func TestAntigravityAppCutRow(t *testing.T) {
	b, _ := hex.DecodeString("0A0C08E8BCB6D60610F09EC7EF021802320C08EDBCB6D60610E08CA0A2023A0C08EDBCB6D60610A09BAAA402420C08EDBCB6D60610A09BAAA4024A7508A60A")
	if _, _, ok := agAppStep(b); ok {
		t.Error("a row cut inside its model call read as a call")
	}
}

// oioio's Antigravity database (#help, 11-06) names gemini-4-argon's
// generator "gemini-4-argon-d": Google's deployment of that model, as
// gemini-3.8-flash-n is 3.8 Flash's. It counts as gemini-4-argon, so Usage
// shows one model at its price. A lone letter after another maker's id, or
// a Gemini id whose last word is longer, stays as written.
func TestAntigravityAppDeploymentName(t *testing.T) {
	gen := func(field int, m string) []byte {
		inner := append(binary.AppendUvarint(nil, uint64(field)<<3|2), byte(len(m)))
		inner = append(inner, m...)
		return append([]byte{1<<3 | 2, byte(len(inner))}, inner...)
	}
	for in, want := range map[string]string{
		"gemini-4-argon-d":      "gemini-4-argon",
		"gemini-3.8-flash-n":    "gemini-3.8-flash",
		"gemini-3.8-flash-lite": "gemini-3.8-flash-lite",
		"gemini-3.8-flash-nano": "gemini-3.8-flash-nano",
		"gemini-4-argon":        "gemini-4-argon",
		"gemini-d":              "gemini-d",
		"gpt-5-d":               "gpt-5-d",
		"claude-sonnet-4-5-n":   "claude-sonnet-4-5-n",
		"gemini-4-argon-D":      "gemini-4-argon-D",
	} {
		for _, field := range []int{19, 21, 22} {
			if got := agAppModel(gen(field, in)); got != want {
				t.Errorf("field %d %q: %q, want %q", field, in, got, want)
			}
		}
	}
}

// antigravitySummariesSchema is conversation_summaries.db as Antigravity
// 2.29.0's language_server makes it in ~/.gemini/antigravity on its first
// start (run in a sandbox HOME, 11-07): agy's table, in the app's folder.
const antigravitySummariesSchema = "CREATE TABLE `conversation_summaries` (`conversation_id` text,`title` text NOT NULL DEFAULT \"\",`preview` text NOT NULL DEFAULT \"\",`step_count` integer NOT NULL DEFAULT 0,`last_modified_time` datetime NOT NULL,`workspace_uris` text NOT NULL,`status` text NOT NULL DEFAULT \"\",`source` text NOT NULL DEFAULT \"\",`project_id` text NOT NULL DEFAULT \"\",`agent_name` text NOT NULL DEFAULT \"\",`parent_conversation_id` text NOT NULL DEFAULT \"\",`nesting_depth` integer NOT NULL DEFAULT 0,`battle_id` text NOT NULL DEFAULT \"\",`winning_conversation_id` text NOT NULL DEFAULT \"\",`not_fully_idle` numeric NOT NULL DEFAULT false,`killed` numeric NOT NULL DEFAULT false,`last_user_input_time` datetime NOT NULL,`last_user_input_step_index` integer NOT NULL DEFAULT -1,`app_data_dir` text NOT NULL DEFAULT \"\",`raw_summary` blob,`group_id` text NOT NULL DEFAULT \"\",PRIMARY KEY (`conversation_id`));\n" +
	"CREATE INDEX `idx_conversation_summaries_last_user_input_time` ON `conversation_summaries`(`last_user_input_time`);\n" +
	"CREATE INDEX `idx_conversation_summaries_last_modified_time` ON `conversation_summaries`(`last_modified_time`);"

// xxooioo (Discord #help, 11-07): every Antigravity app session read "(no
// prompt)". The app's name for a conversation is its row of
// conversation_summaries.db: before a title, preview holds the user's own
// words (agy's real row: title "", preview "hi"), and once the app names
// it, title. A conversation with no row stays unnamed.
func TestAntigravityAppTitle(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	const id, other = "37c3c786-9a11-44be-97df-b8fc1ccebc78", "b9eace9a-b15a-4dde-a440-b52e02016fa5"
	antigravityFixture(t, home, id)
	antigravityFixture(t, home, other)
	db, err := sql.Open("sqlite", filepath.Join(home, ".gemini", "antigravity", "conversation_summaries.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(antigravitySummariesSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO conversation_summaries (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, status, project_id, last_user_input_time, app_data_dir) VALUES (?, '', '修一下登录页的跳转', 2, '2026-11-07 00:12:46.60116+00:00', '[\"file:///work/site\"]', 'CASCADE_RUN_STATUS_IDLE', 'default-project', '2026-11-07 00:12:43.784481+00:00', 'antigravity')", id); err != nil {
		t.Fatal(err)
	}
	ss := List(0)
	if s := find(t, ss, "antigravity", id); s.Title != "修一下登录页的跳转" {
		t.Fatalf("before a title: %q, want the preview", s.Title)
	}
	if s := find(t, ss, "antigravity", other); s.Title != "" {
		t.Fatalf("no row: %q", s.Title)
	}

	if _, err := db.Exec("UPDATE conversation_summaries SET title = 'Login redirect fix', preview = 'Login redirect fix' WHERE conversation_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if s := find(t, List(0), "antigravity", id); s.Title != "Login redirect fix" {
		t.Fatalf("after the app named it: %q", s.Title)
	}
}

// xxooioo (Discord, 11-07): every Antigravity app session sat under "No
// folder". A conversation's folder is its summary row's first local
// workspace URI, as agy's is; a row with none, and a conversation with no
// row, have no folder.
func TestAntigravityAppFolder(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	const site, api, bare, none = "37c3c786-9a11-44be-97df-b8fc1ccebc78", "4f1d2a77-0c3e-4b8e-9d61-2a5e7c90b311", "8e2b6c14-7f5a-4d09-a3c2-61d0f4e9b7a5", "b9eace9a-b15a-4dde-a440-b52e02016fa5"
	for _, id := range []string{site, api, bare, none} {
		antigravityFixture(t, home, id)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, ".gemini", "antigravity", "conversation_summaries.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(antigravitySummariesSchema); err != nil {
		t.Fatal(err)
	}
	for id, ws := range map[string]string{site: `["file:///work/site"]`, api: `["file:///work/api","file:///work/site"]`, bare: `[]`} {
		if _, err := db.Exec("INSERT INTO conversation_summaries (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, status, project_id, last_user_input_time, app_data_dir) VALUES (?, 'A talk', 'A talk', 2, '2026-11-07 00:12:46.60116+00:00', ?, 'CASCADE_RUN_STATUS_IDLE', 'default-project', '2026-11-07 00:12:43.784481+00:00', 'antigravity')", id, ws); err != nil {
			t.Fatal(err)
		}
	}
	ss := List(0)
	for id, want := range map[string]string{site: "/work/site", api: "/work/api", bare: "", none: ""} {
		if s := find(t, ss, "antigravity", id); s.Cwd != want {
			t.Errorf("%s: folder %q, want %q", id, s.Cwd, want)
		}
	}
}

// A parse of an app conversation kept from before its folder was read has
// no folder under the same row, so the cache doesn't keep it.
func TestAntigravityAppParseBeforeFolderReadAgain(t *testing.T) {
	var b bytes.Buffer
	c := cacheFile{Version: cacheVersion, Zone: "z", Parsers: map[string]int{"antigravity": 2}, Files: map[string]*state{"/a.db": {Agent: "antigravity", Size: 1}}}
	if err := encodeCache(&b, c); err != nil {
		t.Fatal(err)
	}
	files, ok := decodeCache(b.Bytes(), "z")
	if !ok {
		t.Fatal("refused")
	}
	if _, kept := files["/a.db"]; kept {
		t.Fatal("a parse from before the side-question fix is kept")
	}
}

// #5803 (MaydayV): a side question (btw) makes a new conversations/<id>.db,
// copies its parent conversation's steps up to the fork step named in
// parent_references (field 2, with the parent conversation's id in field 6),
// and appends its own steps after that. A parent_references row naming the
// conversation's own id is an internal trajectory rotation and skips nothing.
func TestAntigravityAppSideQuestionCountsOnlyItsOwnSteps(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	const (
		parentID = "5e1e111b-491d-4229-b11f-86f2744ff779"
		sideID   = "b96e43aa-eaba-42fa-bb98-bc7ac77f2a70"
	)
	parentPath := antigravityFixture(t, home, parentID)
	sidePath := antigravityFixture(t, home, sideID)

	// Real rows from MaydayV's 5e1e111b-491d-4229-b11f-86f2744ff779.db and
	// b96e43aa-eaba-42fa-bb98-bc7ac77f2a70.db on #5803.
	refStep30, _ := hex.DecodeString("0a2432343133333331612d323735362d343832622d386538362d333339333163303866383364101e1804200f2802322435653165313131622d343931642d343232392d623131662d383666323734346666373739")
	refStep1458, _ := hex.DecodeString("0a2436663863303961382d646562612d343739642d613061652d33376539346230366431393210b20b180420112802322435653165313131622d343931642d343232392d623131662d383666323734346666373739")
	refFork10459, _ := hex.DecodeString("0a2464333264383534342d366334392d343761352d613961382d31376437666533363831333310db511804200f2802322435653165313131622d343931642d343232392d623131662d383666323734346666373739")
	// 5aa41f4e's row 0: field 2 is -1 (uint64 max varint) before a positive fork step.
	refMinusOne, _ := hex.DecodeString("0a2433623933366462382d333233312d343238642d386662302d39323632323732663439366610ffffffffffffffffff011804322435653165313131622d343931642d343232392d623131662d383666323734346666373739")
	step10457, _ := hex.DecodeString("0a0c08cad7c6d70610b8999484011802320c08d4d7c6d70610908988ff013a0c08d4d7c6d70610e0e0a6ea02420c08d4d7c6d70610e0e0a6ea024a7a08890b10ad1418f20128a3cd0630183a28626f742d61383663653666332d653439392d343239322d383965642d64316338623031356566326542210a0973657373696f6e494412142d33373530373633303334333632383935353739488701506b5a177936767861764b7944636d477a3749507371324630416358890b622433323963353562642d323766332d346565322d626438342d393539346134393162366239a201520a2464333264383534342d366334392d343761352d613961382d31376437666533363831333310d95118e026222435653165313131622d343931642d343232392d623131662d383666323734346666373739d201240a100808120c08d4d7c6d70610e8b68bff010a100803120c08d4d7c6d70610d08fa7ea0282020c08d4d7c6d70610e0e0a6ea029a02206538343935343366323837353431373439656134623339636539353463653137")
	step10459, _ := hex.DecodeString("0a0b08e6d7c6d70610b0f8d8191802320c08f1d7c6d7061080e7b9a10258890b622433323963353562642d323766332d346565322d626438342d393539346134393162366239a201520a2464333264383534342d366334392d343761352d613961382d31376437666533363831333310db5118e126222435653165313131622d343931642d343232392d623131662d383666323734346666373739d201120a100808120c08f1d7c6d70610e0b8bea1029a02203366326466336363323434303439303038306232336237326235653962333838")
	step10462, _ := hex.DecodeString("0a0c08f2d7c6d70610a096cd8b021802320c08fcd7c6d70610e0baf99b023a0c08fcd7c6d70610d0e9c8d502420c08fcd7c6d70610d0e9c8d5024a7708890b10dde90618dc0130183a28626f742d34626637616237632d313766362d346333662d626362392d38643733636566626630313542210a0973657373696f6e494412142d3337353037363330333433363238393535373948860150565a1738367678617265384c4c667971747350794e446c75413858890b622433343935366232302d623130362d343538312d626136362d663334353661343739663661a201520a2465623362313336632d343030312d343636342d383632642d30393930363335336233643910de5118e126222462393665343361612d656162612d343266612d626239382d626337616337376632613730d201240a100808120c08fcd7c6d706109099ff9b020a100803120c08fcd7c6d70610f088c9d50282020c08fcd7c6d70610d0e9c8d5029a02206135333934666331313136383466623638613063653535373733353165353138")
	step10464, _ := hex.DecodeString("0a0c08fcd7c6d70610a8a887f8021802320c0885d8c6d70610c8b3b3b7013a0c0885d8c6d70610e0899dd001420c0885d8c6d70610e0899dd0014a7908890b10c41818890128dcd90630183a28626f742d31393462353936332d303064312d343661612d613838302d63633833646433663431646342210a0973657373696f6e494412142d33373530373633303334333632383935353739483350565a175f617678617358664b3554707a37495076377a7a2d416b58890b622433343935366232302d623130362d343538312d626136362d663334353661343739663661a201520a2465623362313336632d343030312d343636342d383632642d30393930363335336233643910e05118e226222462393665343361612d656162612d343266612d626239382d626337616337376632613730d201240a100808120c0885d8c6d70610b8d6b7b7010a100803120c0885d8c6d70610d0b89dd00182020c0885d8c6d70610e0899dd0019a02206639306138323531363334633435363639386235343439623265313261636331")
	genArgon, _ := hex.DecodeString("0a119a010e67656d696e692d342d6172676f6e")

	// Parent conversation has internal trajectory rotations (field 6 == parentID)
	// and step 10457 after them; both before and after step 30/1458 are its own.
	parentDB, err := sql.Open("sqlite", parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parentDB.Close()
	for idx, ref := range [][]byte{refStep30, refStep1458} {
		if _, err := parentDB.Exec("INSERT INTO parent_references (idx, data) VALUES (?, ?)", idx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := parentDB.Exec("INSERT INTO steps (idx, step_type, metadata) VALUES (10457, 15, ?)", step10457); err != nil {
		t.Fatal(err)
	}
	if _, err := parentDB.Exec("INSERT INTO gen_metadata (idx, data, size) VALUES (4960, ?, ?)", genArgon, len(genArgon)); err != nil {
		t.Fatal(err)
	}

	// Side question copies the parent's steps (0..11 and 10457..10459) and
	// parent_references, records the fork at 10459 from parentID, and adds its
	// own calls at 10462 and 10464.
	sideDB, err := sql.Open("sqlite", sidePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sideDB.Close()
	for idx, ref := range [][]byte{refMinusOne, refStep30, refStep1458, refFork10459} {
		if _, err := sideDB.Exec("INSERT INTO parent_references (idx, data) VALUES (?, ?)", idx, ref); err != nil {
			t.Fatal(err)
		}
	}
	for idx, step := range map[int][]byte{10457: step10457, 10459: step10459, 10462: step10462, 10464: step10464} {
		if _, err := sideDB.Exec("INSERT INTO steps (idx, step_type, metadata) VALUES (?, 15, ?)", idx, step); err != nil {
			t.Fatal(err)
		}
	}
	for _, genIdx := range []int{4960, 4961, 4962} {
		if _, err := sideDB.Exec("INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, ?)", genIdx, genArgon, len(genArgon)); err != nil {
			t.Fatal(err)
		}
	}

	var parentCalls, sideCalls []Call
	for _, c := range Calls(time.Time{}) {
		switch c.Session {
		case parentID:
			parentCalls = append(parentCalls, c)
		case sideID:
			sideCalls = append(sideCalls, c)
		}
	}
	if len(parentCalls) != 5 {
		t.Fatalf("parent calls: %d, want all 5 across its own trajectory rotations: %+v", len(parentCalls), parentCalls)
	}
	slices.SortFunc(sideCalls, func(a, b Call) int { return a.Time.Compare(b.Time) })
	if len(sideCalls) != 2 {
		t.Fatalf("side-question calls: %d, want only its 2 own steps after fork step 10459: %+v", len(sideCalls), sideCalls)
	}
	if sideCalls[0].Model != "gemini-4-argon" || sideCalls[0].Tokens != (Tokens{Input: 111837, Output: 220}) ||
		sideCalls[1].Model != "gemini-4-argon" || sideCalls[1].Tokens != (Tokens{Input: 3140, Output: 137, CacheRead: 109788}) {
		t.Fatalf("side-question calls: %+v", sideCalls)
	}

	ss := List(0)
	if s := find(t, ss, "antigravity", parentID); s.Tokens != (Tokens{Input: 13317 + 14184 + 15531 + 4254 + 2605, Output: 621 + 164 + 194 + 276 + 242, CacheRead: 18561 + 108195}) {
		t.Errorf("parent session tokens: %+v", s.Tokens)
	}
	if s := find(t, ss, "antigravity", sideID); s.Tokens != (Tokens{Input: 111837 + 3140, Output: 220 + 137, CacheRead: 109788}) {
		t.Errorf("side-question session tokens: %+v, want only steps > 10459", s.Tokens)
	}
}
