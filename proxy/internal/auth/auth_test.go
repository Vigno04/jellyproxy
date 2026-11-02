package auth

import (
 "net/http/httptest"
 "testing"
)
func TestExtractAndAuthorize(t *testing.T) {
 r:=httptest.NewRequest("GET","/?secret=q",nil); if ExtractSecret(r)!="q" || !IsAuthorized(r,"q") { t.Fatal("query auth failed") }
 r=httptest.NewRequest("GET","/",nil); r.Header.Set("Authorization","Bearer q"); if !IsAuthorized(r,"q") { t.Fatal("bearer auth failed") }
 if !IsAuthorized(r,"") { t.Fatal("open mode failed") }
}
