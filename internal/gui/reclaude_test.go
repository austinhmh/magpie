package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestReClaudeProvidersSwitchAndSettingsSave(t *testing.T) {
	handler := builtinHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_PARTNERS", "off")
	t.Cleanup(provider.ForgetAccounts)
	request := func(method, route, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, route, strings.NewReader(body)))
		return recorder
	}
	check := func(on bool) {
		t.Helper()
		recorder := request(http.MethodGet, "/api/providers", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET providers: %d %s", recorder.Code, recorder.Body)
		}
		var state providersJSON
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state.ReClaude.On != on {
			t.Fatalf("switch was not reflected in the provider list: %+v", state.ReClaude)
		}
		seen := false
		for _, current := range state.Providers {
			if current.ID == "claude" {
				seen = current.Account != nil && current.Account.ReClaude
			}
		}
		if on && !seen {
			t.Fatal("ReClaude is on, but its synthetic Claude account is missing")
		}
	}
	check(false)
	if recorder := request(http.MethodPost, "/api/providers/reclaude", `{"on":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("switch on: %d %s", recorder.Code, recorder.Body)
	}
	check(true)
	// A general Settings save must not undo the source chosen in Providers.
	if recorder := request(http.MethodPost, "/api/settings", `{"theme":"light","lang":"en"}`); recorder.Code != http.StatusOK {
		t.Fatalf("settings save: %d %s", recorder.Code, recorder.Body)
	}
	if !settings.Load().ReClaude {
		t.Fatal("saving another setting switched ReClaude off")
	}
	if recorder := request(http.MethodPost, "/api/providers/reclaude", `{"on":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("switch off: %d %s", recorder.Code, recorder.Body)
	}
	check(false)
}
