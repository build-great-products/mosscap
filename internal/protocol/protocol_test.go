package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func validHello() Request {
	return Request{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ClientKind: "cli", ClientVersion: "test", Command: "hello", Params: []byte(`{"supported_versions":[1],"capabilities":["status"]}`)}
}
func TestRequestGolden(t *testing.T) {
	got, err := EncodeRequest(validHello())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"test","command":"hello","params":{"supported_versions":[1],"capabilities":["status"]}}`
	if string(got) != want {
		t.Fatalf("\ngot: %s\nwant: %s", got, want)
	}
}
func TestResponseGolden(t *testing.T) {
	r := Response{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ServerVersion: "test", ResultType: "hello", Result: []byte(`{"selected_version":1,"capabilities":["status"]}`)}
	got, err := EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","server_version":"test","result_type":"hello","result":{"selected_version":1,"capabilities":["status"]}}`
	if string(got) != want {
		t.Fatalf("\ngot: %s\nwant: %s", got, want)
	}
}

func TestAdditiveCapabilitiesAreAdvertisedWithoutProtocolBump(t *testing.T) {
	if Version != 1 {
		t.Fatalf("additive commands unexpectedly changed protocol version to %d", Version)
	}
	for _, capability := range []string{CapabilityTrustedUsers, CapabilityConfigReload} {
		found := false
		for _, advertised := range ServerCapabilities {
			found = found || advertised == capability
		}
		if !found {
			t.Fatalf("server does not advertise %q", capability)
		}
	}
}

func TestTrustAndReloadRequestGolden(t *testing.T) {
	for _, tc := range []struct {
		command string
		params  string
	}{
		{"trust.list", `{}`},
		{"trust.add", `{"slack_user_id":"U01234567"}`},
		{"trust.remove", `{"slack_user_id":"U01234567"}`},
		{"config.reload", `{}`},
	} {
		t.Run(tc.command, func(t *testing.T) {
			r := Request{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ClientKind: "cli", ClientVersion: "test", Command: tc.command, Params: []byte(tc.params)}
			got, err := EncodeRequest(r)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"test","command":"` + tc.command + `","params":` + tc.params + `}`
			if string(got) != want {
				t.Fatalf("\ngot: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestTrustAndReloadParamsStrict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		params  string
		valid   bool
	}{
		{"list empty", "trust.list", `{}`, true},
		{"reload empty", "config.reload", `{}`, true},
		{"task reply", "task.reply", `{"task":"abcdefghijklmnopqrstuv","text":"reply","done":true}`, true},
		{"task reply bare selector", "task.reply", `{"task":"abcdefgh","text":"reply","done":true}`, true},
		{"task reply missing text", "task.reply", `{"task":"abcdefghijklmnopqrstuv"}`, false},
		{"add valid immutable ID", "trust.add", `{"slack_user_id":"U01234567"}`, true},
		{"remove valid enterprise ID", "trust.remove", `{"slack_user_id":"W01234567"}`, true},
		{"empty params reject fields", "trust.list", `{"extra":true}`, false},
		{"reload reject fields", "config.reload", `{"extra":true}`, false},
		{"missing user ID", "trust.add", `{}`, false},
		{"unknown mutation field", "trust.remove", `{"slack_user_id":"U01234567","extra":true}`, false},
		{"lowercase user ID", "trust.add", `{"slack_user_id":"u01234567"}`, false},
		{"short user ID", "trust.add", `{"slack_user_id":"U0123456"}`, false},
		{"invalid user ID punctuation", "trust.add", `{"slack_user_id":"U0123-567"}`, false},
		{"oversized user ID", "trust.add", `{"slack_user_id":"U0123456789012345678901234567890123456789012345678901234567890123"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateParams(tc.command, []byte(tc.params))
			if (err == nil) != tc.valid {
				t.Fatalf("ValidateParams(%q, %s) error = %v, valid = %v", tc.command, tc.params, err, tc.valid)
			}
		})
	}
}

func TestTrustAndReloadResultGolden(t *testing.T) {
	trust := TrustMutationDTO{SlackUserID: "U01234567", Trusted: true, Changed: true}
	if got := mustJSON(t, trust); got != `{"slack_user_id":"U01234567","trusted":true,"changed":true}` {
		t.Fatalf("trust result = %s", got)
	}
	users := TrustedUserListDTO{SlackUserIDs: []string{"U01234567", "U76543210"}, SlackUserNames: map[string]string{"U01234567": "george", "U76543210": "moss"}}
	if got := mustJSON(t, users); got != `{"slack_user_ids":["U01234567","U76543210"],"slack_user_names":{"U01234567":"george","U76543210":"moss"}}` {
		t.Fatalf("trusted-user list = %s", got)
	}
	reload := ConfigReloadDTO{Activated: true, SlackNetworkVerificationDeferred: true, ActiveConfigGeneration: 3, ActiveConfigDigest: "sha256:abc"}
	if got := mustJSON(t, reload); got != `{"activated":true,"slack_network_verification_deferred":true,"active_config_generation":3,"active_config_digest":"sha256:abc"}` {
		t.Fatalf("reload result = %s", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestFrameFragmentedAndMultiple(t *testing.T) {
	var wire bytes.Buffer
	codec := NewCodec(&wire, &wire, 64)
	if err := codec.WriteFrame([]byte(`{"one":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := codec.WriteFrame([]byte(`{"two":2}`)); err != nil {
		t.Fatal(err)
	}
	fragmented := &oneByteReader{r: bytes.NewReader(wire.Bytes())}
	read := NewCodec(fragmented, io.Discard, 64)
	for _, want := range []string{`{"one":1}`, `{"two":2}`} {
		got, err := read.ReadFrame()
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v", got, err)
		}
	}
}
func TestFrameRejectsBadLengthsAndUTF8(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"zero", []byte{0, 0, 0, 0}, ErrEmptyFrame},
		{"large", []byte{0, 0, 0, 65}, ErrFrameTooLarge},
		{"utf8", append([]byte{0, 0, 0, 1}, 0xff), ErrInvalidUTF8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCodec(bytes.NewReader(tc.data), io.Discard, 64).ReadFrame()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
func TestPhase1ParamsStrictAndRoundTrip(t *testing.T) {
	cases := []struct{ command, params string }{
		{"slack.history", `{"limit":100}`},
		{"slack.thread", `{"thread_ts":"1712345678.000001"}`},
		{"slack.send", `{"thread_ts":"1712345678.000001","text":"hello","task":"abc"}`},
		{"slack.edit", `{"message_ts":"1712345678.000001","text":"hello"}`},
		{"slack.delete", `{"message_ts":"1712345678.000001","reason":"operator request"}`},
		{"slack.react", `{"message_ts":"1712345678.000001","emoji":"eyes"}`},
		{"slack.unreact", `{"message_ts":"1712345678.000001","emoji":"eyes"}`},
		{"effect.list", `{"state":"ambiguous","limit":100}`},
		{"effect.inspect", `{"effect":"eff_abc"}`},
		{"effect.retry", `{"effect":"eff_abc","reason":"reviewed","confirm_duplicate_risk":true}`},
		{"file.show", `{"file":"fil_abc"}`},
		{"file.get", `{"file":"fil_AbC-_123"}`},
		{"file.release", `{"transfer_id":"xfr_AbC-_123"}`},
		{"slack.file", `{"file_path":"/tmp/source","comment":"one"}`},
		{"slack.send", `{"text":"hello","file_paths":["/tmp/one","/tmp/two"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			r := Request{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ClientKind: "cli", ClientVersion: "test", Command: tc.command, Params: []byte(tc.params)}
			encoded, err := EncodeRequest(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRequest(encoded); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct{ command, params string }{
		{"slack.history", `{"limit":101}`}, {"slack.thread", `{"thread_ts":"bad"}`},
		{"slack.send", `{"text":"","extra":true}`}, {"slack.edit", `{"message_ts":"1.000001","text":"x","thread_ts":"1.000001"}`},
		{"slack.delete", `{"message_ts":"1.000001","reason":""}`}, {"slack.react", `{"message_ts":"1.000001","emoji":"bad emoji"}`},
		{"effect.list", `{"state":"unknown"}`}, {"effect.inspect", `{"effect":"tsk_abc"}`},
		{"effect.retry", `{"effect":"eff_abc","reason":""}`}, {"file.show", `{"file":"/tmp/file"}`},
		{"file.get", `{"file":"fil_abc","bytes":"forbidden"}`}, {"file.release", `{"transfer_id":"fil_abc"}`},
		{"slack.file", `{"file_path":"relative"}`}, {"slack.send", `{"text":"ok","file_paths":["/tmp/a"],"channel":"CINJECT"}`},
	} {
		if err := ValidateParams(tc.command, []byte(tc.params)); err == nil {
			t.Fatalf("accepted invalid %s %s", tc.command, tc.params)
		}
	}
}

func TestPhase1CapabilitiesAndSafeDTOs(t *testing.T) {
	for _, capability := range []string{CapabilitySlackRuntime, CapabilitySlackReads, CapabilitySlackEffects, CapabilitySlackSourceMetadata, CapabilityFileTransfer, CapabilityTaskReply} {
		found := false
		for _, advertised := range ServerCapabilities {
			found = found || advertised == capability
		}
		if !found {
			t.Fatalf("missing Phase 1 capability %q", capability)
		}
	}
	// Effect list/mutation DTOs intentionally lack request text and reasons;
	// inspect is the one explicit result type which can contain exact intent.
	list := EffectListDTO{Effects: []SlackEffectDTO{{EffectID: "eff_abc", Kind: "send", State: "confirmed", PreparedAt: "2026-01-01T00:00:00Z"}}}
	if got := mustJSON(t, list); got != `{"effects":[{"effect_id":"eff_abc","kind":"send","state":"confirmed","attempt_count":0,"prepared_at":"2026-01-01T00:00:00Z"}]}` {
		t.Fatalf("effect list = %s", got)
	}
	inspect := EffectInspectDTO{Effect: list.Effects[0], Intent: SlackEffectIntentDTO{Text: "explicit body"}}
	if got := mustJSON(t, inspect); got == "" {
		t.Fatal("empty inspect DTO")
	}
	if _, err := EncodeResponse(NewErrorResponse("req_abcdefghijklmnopqrstuv", "test", "external", "external_failed", "xoxb-secret-token", false)); err != nil {
		t.Fatal(err)
	}
	unsafe := Error{Code: "x", Category: "usage", Message: "Authorization: Bearer secret"}
	if err := unsafe.Validate(); err == nil {
		t.Fatal("accepted credential-bearing error")
	}
}

func TestStrictDecode(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[2],"capabilities":[]}}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]},"unknown":1}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]}} {}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":null}`),
	}
	for _, payload := range cases {
		if _, err := DecodeRequest(payload); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}
func TestMetadataPurgeParamsStrictAndNormalize(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"2026-01-02T03:04:05.123456Z", true},
		{"2026-01-02T04:04:05.123456+01:00", true},
		{"2026-01-02", false}, {"2026-01-02T03:04:05", false},
		{"2026-01-02T03:04:05Z", false}, {"2026-01-02T03:04:05.1Z", false},
		{"2026-01-02T03:04:05.12345Z", false}, {"2026-01-02T03:04:05,123456Z", false},
		{"2026-01-02T03:04:05.123456", false}, {"2026-01-02T03:04:05.1234567Z", false}, {"not-a-time", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := ParsePurgeCutoff(tc.value)
			if (err == nil) != tc.valid {
				t.Fatalf("ParsePurgeCutoff(%q) = %v", tc.value, err)
			}
		})
	}
	if err := ValidateParams("metadata.purge", []byte(`{"cutoff":"2026-01-02T03:04:05.000000Z","mode":"dry_run"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"cutoff":"2026-01-02T03:04:05","mode":"dry_run"}`, `{"cutoff":"2026-01-02T03:04:05Z","mode":"other"}`, `{"cutoff":"2026-01-02T03:04:05,1Z","mode":"dry_run"}`, `{"cutoff":"2026-01-02T03:04:05Z","mode":"dry_run","extra":true}`} {
		if err := ValidateParams("metadata.purge", []byte(raw)); err == nil {
			t.Fatalf("accepted invalid params %s", raw)
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]}}`))
	f.Fuzz(func(t *testing.T, payload []byte) { _, _ = DecodeRequest(payload) })
}
func FuzzFrameRead(f *testing.F) {
	seed := make([]byte, 4)
	binary.BigEndian.PutUint32(seed, 2)
	seed = append(seed, '{', '}')
	f.Add(seed)
	f.Fuzz(func(t *testing.T, wire []byte) { _, _ = NewCodec(bytes.NewReader(wire), io.Discard, 1024).ReadFrame() })
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}
