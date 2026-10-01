package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
)

func TestLoginFlowWithVerifyCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.ilink.SetLoginStates("wait", "scaned", "need_verifycode", "confirmed")
	resp, _ := e.post(t, "/login/start", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start %d", resp.StatusCode)
	}
	status := func() LoginStatus {
		_, body := e.get(t, "/login/status")
		var s LoginStatus
		json.Unmarshal([]byte(body), &s)
		return s
	}
	eventually(t, func() bool { return status().State == "need_verify" })
	if s := status(); !s.HasQR {
		t.Fatalf("status %+v", s)
	}
	resp2, _ := e.client().Get(e.srv.URL + "/login/qr.png")
	resp2.Body.Close()
	if resp2.StatusCode != 200 || resp2.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr %d", resp2.StatusCode)
	}
	// 流程进行中再点一次不会开第二个
	e.post(t, "/login/start", nil)
	e.post(t, "/login/verify", url.Values{"code": {e.ilink.VerifyCode}})
	eventually(t, func() bool { return status().State == "confirmed" })
	if cred, ok, _ := e.sess.Cred(ctx); !ok || cred.UserID != ilinktest.OwnerID || !cred.LoginAt.Equal(e.clk.Now()) {
		t.Fatalf("cred %+v", cred)
	}
	_, body := e.get(t, "/login")
	mustContain(t, body, "当前微信连接：正常", "登录成功")
	mustNotContain(t, body, ilinktest.Token)
}

func TestLoginAlreadyBound(t *testing.T) {
	e := newEnv(t)
	e.ilink.SetLoginStates("binded_redirect")
	e.post(t, "/login/start", nil)
	eventually(t, func() bool {
		_, body := e.get(t, "/login/status")
		return strings.Contains(body, "login --import")
	})
}
