package protocol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const deliveryID = "AbCdEfGhIjKlMnOpQrStUv"
const deliveryKey = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDeliveryCapabilityIsDefinedButNotAdvertised(t *testing.T) {
	if CapabilityPiDeliveryMVP != "pi_delivery_mvp" {
		t.Fatalf("capability=%q", CapabilityPiDeliveryMVP)
	}
	for _, c := range ServerCapabilities {
		if c == CapabilityPiDeliveryMVP {
			t.Fatal("foundation must not advertise incomplete delivery capability")
		}
	}
}

func TestDeliveryParamsAreStrictAndBounded(t *testing.T) {
	validOpen := `{"pi_session_id":"session-1","observed":[{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `"}]}`
	cases := []struct {
		command, raw string
		valid        bool
	}{
		{"delivery.open", validOpen, true},
		{"delivery.open", `{"pi_session_id":"session-1","extra":true}`, false},
		{"delivery.open", `{"pi_session_id":""}`, false},
		{"delivery.open", `{"pi_session_id":"session-1","observed":[{"delivery_id":"` + deliveryID + `","delivery_key":"bad"}]}`, false},
		{"delivery.poll", `{}`, true}, {"delivery.poll", `{"pi_session_id":"session-1"}`, false},
		{"delivery.inserting", `{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `"}`, true},
		{"delivery.persisted", `{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `"}`, true},
		{"delivery.rejected", `{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `","diagnostic_code":"oversized_task"}`, true},
		{"delivery.rejected", `{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `","diagnostic_code":"bad-code"}`, false},
		{"delivery.close", `{}`, true}, {"delivery.close", `{"extra":true}`, false},
		{"delivery.list", `{"state":"uncertain"}`, true}, {"delivery.list", `{"state":"lost"}`, false},
		{"delivery.inspect", `{"delivery":"dlv_AbCd"}`, true}, {"delivery.inspect", `{"delivery":"invalid!"}`, false},
		{"delivery.requeue", `{"delivery":"dlv_AbCd","reason":"explicit operator recovery"}`, true},
	}
	for _, tc := range cases {
		if err := ValidateParams(tc.command, []byte(tc.raw)); (err == nil) != tc.valid {
			t.Errorf("%s %s: err=%v valid=%t", tc.command, tc.raw, err, tc.valid)
		}
	}
	observed := make([]string, 257)
	for i := range observed {
		observed[i] = `{"delivery_id":"` + deliveryID + `","delivery_key":"` + deliveryKey + `"}`
	}
	if err := ValidateParams("delivery.open", []byte(`{"pi_session_id":"s","observed":[`+strings.Join(observed, ",")+`]}`)); err == nil {
		t.Fatal("accepted over-cap observations")
	}
}

func TestDeliveryGoldenFrames(t *testing.T) {
	b, err := os.ReadFile("testdata/delivery-v1-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct{ Name, Direction, Frame string }
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.Direction == "request" {
			if _, err := DecodeRequest([]byte(fixture.Frame)); err != nil {
				t.Errorf("%s request: %v", fixture.Name, err)
			}
		} else if _, err := DecodeResponse([]byte(fixture.Frame)); err != nil {
			t.Errorf("%s response: %v", fixture.Name, err)
		}
	}
}
