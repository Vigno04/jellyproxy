package abr

import "testing"

func TestFairShareAndDeduplication(t *testing.T) {
 m:=NewBandwidthManager(Config{Enabled:true,UploadSpeedBps:10_000_000,HeadroomPercent:0})
 if got:=m.Allocate("a","movie",8_000_000);got!=8_000_000 {t.Fatalf("first allocation=%d",got)}
 if got:=m.Allocate("b","movie",8_000_000);got!=8_000_000 {t.Fatalf("dedup allocation=%d",got)}
 m.Allocate("c","other",8_000_000)
 if got:=m.GetStatus().UsedBps;got!=10_000_000 {t.Fatalf("used=%d",got)}
}
func TestFCFSAndRelease(t *testing.T) { m:=NewBandwidthManager(Config{Enabled:true,UploadSpeedBps:10_000_000,Policy:"FirstComeFirstServed"}); m.Allocate("a","a",8_000_000); if got:=m.Allocate("b","b",8_000_000);got!=2_000_000 {t.Fatalf("fcfs=%d",got)}; m.Release("a"); if m.GetStatus().ActivePipes!=1 {t.Fatal("release did not remove pipe")} }
func TestDefaultsAndDisabled(t *testing.T) {m:=NewBandwidthManager(Config{}); if m.Config().MinBitrateBps!=1500000||m.Config().Policy!="FairShare" {t.Fatal("defaults")}; if got:=m.Allocate("a","x",9_000_000);got!=9_000_000 {t.Fatal("disabled allocation")}}
