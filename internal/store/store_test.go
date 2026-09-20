package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore 返回使用临时数据库、时钟可控的 Store。
func newTestStore(t *testing.T) (*Store, func(time.Time)) {
	t.Helper()
	clock := time.Date(2026, 9, 20, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "labels.sqlite3"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return clock }
	return s, func(at time.Time) { clock = at }
}

func validCredential(validUntil string) AuthorizationInput {
	return AuthorizationInput{
		CredentialID:     "CRED-1",
		ProducerRef:      "MAKER-DEMO",
		ProducerName:     "曹县示范木业有限公司",
		ProductRef:       "SPEC-PLY-18",
		SpecName:         "18mm 杉木生态板",
		BatchRef:         "WOOD-DEMO",
		InspectionDigest: "sha256:demo-inspection",
		ValidUntil:       validUntil,
		IssuedAt:         "2026-01-01T00:00:00+08:00",
	}
}

// seedHappyPath 登记凭证、印批、装箱、交付、领用；返回已领用编号与未领用编号。
func seedHappyPath(t *testing.T, ctx context.Context, s *Store) (issuedCode, spareCode string) {
	t.Helper()
	if _, err := s.ImportAuthorization(ctx, validCredential("2026-12-31T23:59:59+08:00")); err != nil {
		t.Fatalf("收录凭证失败: %v", err)
	}
	if _, err := s.RegisterPrintBatch(ctx, PrintBatchInput{
		BatchID: "PB-1", PrinterRef: "PRESS-1", PrinterName: "曹县标签印厂",
		ProductRef: "SPEC-PLY-18", SpecName: "18mm 杉木生态板",
		PrintedAt: "2026-09-01T09:00:00+08:00",
		Codes:     []string{"L-1", "L-2", "L-3", "L-4"},
	}); err != nil {
		t.Fatalf("登记印批失败: %v", err)
	}
	if _, err := s.PackBox(ctx, BoxPackInput{
		BoxID: "BOX-1", PrintBatchID: "PB-1", Codes: []string{"L-1", "L-2"},
		PackedAt: "2026-09-02T09:00:00+08:00",
	}); err != nil {
		t.Fatalf("装箱失败: %v", err)
	}
	if _, err := s.DeliverBox(ctx, BoxDeliverInput{
		BoxID: "BOX-1", DeliveredToProducerRef: "MAKER-DEMO",
		DeliveredToProducerName: "曹县示范木业有限公司", CourierRef: "COURIER-1",
		DeliveredAt: "2026-09-03T09:00:00+08:00",
	}); err != nil {
		t.Fatalf("交付失败: %v", err)
	}
	if _, err := s.Issue(ctx, IssueInput{
		CredentialID: "CRED-1", Codes: []string{"L-1"},
		IssuedAt: "2026-09-05T09:00:00+08:00",
	}); err != nil {
		t.Fatalf("领用失败: %v", err)
	}
	return "L-1", "L-2"
}

func TestHappyPathAndVerify(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	code, _ := seedHappyPath(t, ctx, s)

	res, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1001",
		EventTime: "2026-09-10T08:30:00+08:00",
		Items:     []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-A"}},
	})
	if err != nil {
		t.Fatalf("贴附失败: %v", err)
	}
	if len(res.Affected) != 1 || res.Affected[0] != code {
		t.Fatalf("贴附结果异常: %+v", res)
	}

	v, err := s.Verify(ctx, code)
	if err != nil {
		t.Fatalf("验真失败: %v", err)
	}
	if !v.Valid || v.Status != "applied" || v.Anomaly != "" {
		t.Fatalf("已贴附编号应当验真通过且无异常: %+v", v)
	}
	if v.ProducerName != "曹县示范木业有限公司" || v.SpecName != "18mm 杉木生态板" {
		t.Fatalf("验真应展示企业名称与规格: %+v", v)
	}
	if v.ProductBatchRef != "PLANK-BATCH-A" || v.Credential == nil {
		t.Fatalf("验真应展示产品批次与凭证: %+v", v)
	}
}

// 离线设备重传：同键重放必须幂等，且改贴另一个批次必须被拒绝。
func TestOfflineApplyIdempotentAndNoDoubleBinding(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	code, _ := seedHappyPath(t, ctx, s)

	first := ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1001",
		EventTime: "2026-09-10T08:30:00+08:00",
		Items:     []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-A"}},
	}
	if _, err := s.Apply(ctx, first); err != nil {
		t.Fatalf("首次贴附失败: %v", err)
	}
	// 网络恢复后整包重传。
	replay, err := s.Apply(ctx, first)
	if err != nil {
		t.Fatalf("同事件重传应幂等成功: %v", err)
	}
	if len(replay.Affected) != 0 || len(replay.Skipped) != 1 {
		t.Fatalf("重传应全部跳过: %+v", replay)
	}
	var eventCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM label_events WHERE code=? AND event_type='applied'`, code).
		Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("重传不得产生第二条贴附台账，实际 %d 条", eventCount)
	}

	// 同一设备事件键却声称贴到别的批次：拒绝。
	tampered := first
	tampered.Items = []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-B"}}
	if _, err := s.Apply(ctx, tampered); !errors.Is(err, ErrConflict) {
		t.Fatalf("同键改贴批次应冲突，得到 %v", err)
	}
	// 换新事件键再贴别的批次：一个码不能属于两个批次。
	if _, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1002",
		EventTime: "2026-09-11T08:30:00+08:00",
		Items:     []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-B"}},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("编号二次贴附应冲突，得到 %v", err)
	}
}

// 授权凭证是不可修改的外部依据：同内容可重复收录，改内容被拒绝，库层禁止 UPDATE/DELETE。
func TestAuthorizationIsImmutable(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	created, err := s.ImportAuthorization(ctx, validCredential("2026-12-31T23:59:59+08:00"))
	if err != nil || !created {
		t.Fatalf("首次收录应 created=true, got %v %v", created, err)
	}
	again, err := s.ImportAuthorization(ctx, validCredential("2026-12-31T23:59:59+08:00"))
	if err != nil || again {
		t.Fatalf("同内容重复收录应幂等 created=false, got %v %v", again, err)
	}
	changed := validCredential("2027-12-31T23:59:59+08:00") // 试图延长有效期
	if _, err := s.ImportAuthorization(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("不同内容覆盖凭证应冲突，得到 %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE authorizations SET valid_until='2099-01-01T00:00:00+08:00' WHERE credential_id='CRED-1'`); err == nil {
		t.Fatal("数据库层应禁止修改授权凭证")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM authorizations WHERE credential_id='CRED-1'`); err == nil {
		t.Fatal("数据库层应禁止删除授权凭证")
	}
}

// 授权到期：此前已售出的合规板材验真仍通过；未领用/已领用未贴附的剩余标签被冻结停用。
func TestExpiryFreezesUnusedButKeepsSoldValid(t *testing.T) {
	ctx := context.Background()
	s, advance := newTestStore(t)
	issued, spare := seedHappyPath(t, ctx, s)

	// 到期前完成贴附（视为已售出）。
	if _, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1",
		EventTime: "2026-10-01T08:00:00+08:00",
		Items:     []ApplyItem{{Code: issued, ProductBatchRef: "PLANK-BATCH-A"}},
	}); err != nil {
		t.Fatalf("到期前贴附失败: %v", err)
	}
	// 再领用一张但暂不贴附。
	if _, err := s.Issue(ctx, IssueInput{
		CredentialID: "CRED-1", Codes: []string{spare},
		IssuedAt: "2026-10-02T08:00:00+08:00",
	}); err != nil {
		t.Fatalf("领用备用标签失败: %v", err)
	}

	// 时间越过授权有效期。
	advance(time.Date(2027, 1, 2, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)))

	if _, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-2",
		EventTime: "2027-01-02T08:00:00+08:00",
		Items:     []ApplyItem{{Code: spare, ProductBatchRef: "PLANK-BATCH-X"}},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("授权到期后剩余标签不得继续贴附，得到 %v", err)
	}

	res, err := s.FreezeExpired(ctx)
	if err != nil {
		t.Fatalf("冻结到期标签失败: %v", err)
	}
	if len(res.Affected) != 1 || res.Affected[0] != spare {
		t.Fatalf("应仅冻结已领用未贴附的 %s，实际 %+v", spare, res.Affected)
	}

	sold, err := s.Verify(ctx, issued)
	if err != nil {
		t.Fatalf("验真已售标签失败: %v", err)
	}
	if !sold.Valid || sold.Anomaly != "" {
		t.Fatalf("授权到期不应让此前售出的合规板材变成假货: %+v", sold)
	}
	if sold.Credential == nil || sold.Credential.AuthorizationState != "expired" || sold.Note == "" {
		t.Fatalf("已售标签应说明凭证已到期但贴附在先: %+v", sold)
	}

	frozen, err := s.Verify(ctx, spare)
	if err != nil {
		t.Fatalf("验真冻结标签失败: %v", err)
	}
	if frozen.Valid || frozen.AnomalyCode != "frozen" {
		t.Fatalf("未使用标签应显示冻结异常: %+v", frozen)
	}
}

// 已交付但未领用的标签在授权到期后也必须停止使用（同范围已领用未贴附标签一并冻结）。
func TestExpiryFreezesDeliveredUnissued(t *testing.T) {
	ctx := context.Background()
	s, advance := newTestStore(t)
	_, spare := seedHappyPath(t, ctx, s)
	advance(time.Date(2027, 1, 2, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)))

	res, err := s.FreezeExpired(ctx)
	if err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	got := map[string]bool{}
	for _, c := range res.Affected {
		got[c] = true
	}
	if !got["L-1"] || !got[spare] {
		t.Fatalf("已领用未贴附 L-1 与已交付未领用 L-2 都应冻结，实际 %+v", res.Affected)
	}
	if _, err := s.Issue(ctx, IssueInput{CredentialID: "CRED-1", Codes: []string{spare}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("冻结标签不得再领用，得到 %v", err)
	}
}

// 整批遗失：品牌方可查每个挂失编号的最后保管人；挂失编号公众扫码必须看到异常。
func TestBatchLostTracesLastCustodian(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	issued, _ := seedHappyPath(t, ctx, s)

	// 企业领走后丢失 L-1。
	if _, err := s.ReportLost(ctx, ReportInput{
		Codes: []string{issued}, ActorType: "producer", ActorRef: "MAKER-DEMO",
		ActorName: "曹县示范木业有限公司", EventTime: "2026-09-20T12:00:00+08:00",
		Note: "车间盘点缺失",
	}); err != nil {
		t.Fatalf("企业挂失失败: %v", err)
	}
	// 印厂尚未装箱的散张 L-3 在印厂环节丢失。
	if _, err := s.ReportLost(ctx, ReportInput{
		Codes: []string{"L-3"}, ActorType: "printer", ActorRef: "PRESS-1",
		ActorName: "曹县标签印厂", EventTime: "2026-09-01T18:00:00+08:00",
	}); err != nil {
		t.Fatalf("印厂挂失失败: %v", err)
	}

	trace, err := s.TraceBatch(ctx, "PB-1")
	if err != nil {
		t.Fatalf("批次溯源失败: %v", err)
	}
	if trace.Counts["lost_flagged"] != 2 {
		t.Fatalf("应有 2 个挂失标记，统计为 %+v", trace.Counts)
	}
	custodians := map[string]string{}
	for _, l := range trace.Lost {
		custodians[l.Code] = l.LastCustodian.Ref
	}
	if custodians["L-1"] != "MAKER-DEMO" || custodians["L-3"] != "PRESS-1" {
		t.Fatalf("最后保管人定位错误: %+v", custodians)
	}

	v, err := s.Verify(ctx, issued)
	if err != nil {
		t.Fatalf("验真失败: %v", err)
	}
	// 即使标签上仍印着/绑定原企业名称，公众必须同时看到挂失异常。
	if v.Valid || v.AnomalyCode != "lost" || v.ProducerName == "" {
		t.Fatalf("挂失编号应保留企业信息但显著标识异常: %+v", v)
	}

	// 寻回后异常解除，标签可继续贴附。
	if _, err := s.ReportRecovered(ctx, ReportInput{
		Codes: []string{issued}, ActorType: "producer", ActorRef: "MAKER-DEMO",
		EventTime: "2026-09-21T09:00:00+08:00",
	}); err != nil {
		t.Fatalf("寻回失败: %v", err)
	}
	v2, _ := s.Verify(ctx, issued)
	if v2.AnomalyCode == "lost" {
		t.Fatalf("寻回后不应再显示挂失异常: %+v", v2)
	}
}

// 未贴附标签流入公众视野时显示"尚未贴附"异常，作废标签显示作废异常。
func TestPublicAnomalies(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	_, spare := seedHappyPath(t, ctx, s)

	notApplied, err := s.Verify(ctx, spare)
	if err != nil {
		t.Fatalf("验真失败: %v", err)
	}
	if notApplied.Valid || notApplied.AnomalyCode != "not_applied" {
		t.Fatalf("未贴附编号应提示未贴附异常: %+v", notApplied)
	}
	if notApplied.SpecName != "18mm 杉木生态板" {
		t.Fatalf("未贴附标签也应展示预印规格: %+v", notApplied)
	}

	if _, err := s.Void(ctx, VoidInput{
		Codes: []string{"L-4"}, Reason: "印制残损", ActorRef: "brand-admin",
		EventTime: "2026-09-04T09:00:00+08:00",
	}); err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	voided, err := s.Verify(ctx, "L-4")
	if err != nil {
		t.Fatalf("验真失败: %v", err)
	}
	if voided.Valid || voided.AnomalyCode != "voided" {
		t.Fatalf("作废编号应显示作废异常: %+v", voided)
	}
}

// 已贴附售出的编号不得被作废或挂失，保护既有销售。
func TestAppliedLabelsCannotBeVoidedOrLost(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	code, _ := seedHappyPath(t, ctx, s)
	if _, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1",
		EventTime: "2026-09-10T08:30:00+08:00",
		Items:     []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-A"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Void(ctx, VoidInput{Codes: []string{code}, Reason: "误操作"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("已贴附编号不得作废，得到 %v", err)
	}
	if _, err := s.ReportLost(ctx, ReportInput{
		Codes: []string{code}, ActorRef: "MAKER-DEMO",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("已贴附编号不得挂失，得到 %v", err)
	}
}

// 领用必须凭有效凭证，且规格与标签预印规格一致、标签确由该企业保管。
func TestIssueGuards(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	code, _ := seedHappyPath(t, ctx, s)

	// 规格不符的凭证。
	wrong := validCredential("2026-12-31T23:59:59+08:00")
	wrong.CredentialID = "CRED-2"
	wrong.ProductRef = "SPEC-OTHER"
	wrong.SpecName = "其他规格"
	if _, err := s.ImportAuthorization(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Issue(ctx, IssueInput{CredentialID: "CRED-2", Codes: []string{"L-2"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("规格不符应拒绝领用，得到 %v", err)
	}
	// 未入库凭证不能领用。
	if _, err := s.Issue(ctx, IssueInput{CredentialID: "NO-SUCH", Codes: []string{"L-2"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的凭证应 NotFound，得到 %v", err)
	}
	// 印厂环节的标签不能被企业直接领用。
	if _, err := s.Issue(ctx, IssueInput{CredentialID: "CRED-1", Codes: []string{"L-3"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("未交付标签不能领用，得到 %v", err)
	}
	if code == "" {
		t.Fatal("sanity")
	}
}

// 凭证到期后企业取得续期凭证：此前贴附的板材验真不应被误报为异常。
func TestRenewedCredentialKeepsSoldLabelClean(t *testing.T) {
	ctx := context.Background()
	s, advance := newTestStore(t)
	code, _ := seedHappyPath(t, ctx, s)
	if _, err := s.Apply(ctx, ApplyInput{
		DeviceID: "DEV-9", DeviceEventID: "EVT-1",
		EventTime: "2026-10-01T08:00:00+08:00",
		Items:     []ApplyItem{{Code: code, ProductBatchRef: "PLANK-BATCH-A"}},
	}); err != nil {
		t.Fatal(err)
	}
	advance(time.Date(2027, 1, 2, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)))
	// 新年度凭证入库（新编号、新有效期），覆盖同一企业 + 规格。
	renewed := validCredential("2027-12-31T23:59:59+08:00")
	renewed.CredentialID = "CRED-2"
	if _, err := s.ImportAuthorization(ctx, renewed); err != nil {
		t.Fatalf("续期凭证收录失败: %v", err)
	}
	v, err := s.Verify(ctx, code)
	if err != nil {
		t.Fatalf("验真失败: %v", err)
	}
	if !v.Valid || v.Anomaly != "" || v.Note != "" {
		t.Fatalf("续期后已售标签应干净通过: %+v", v)
	}
	if v.Credential == nil || v.Credential.AuthorizationState != "active" {
		t.Fatalf("续期后授权状态应为 active: %+v", v.Credential)
	}
}

// 同一验证编号不得在两个印制批次中复用。
func TestCodeCannotBelongToTwoPrintBatches(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if _, err := s.RegisterPrintBatch(ctx, PrintBatchInput{
		BatchID: "PB-1", PrinterRef: "PRESS-1", ProductRef: "SPEC-PLY-18",
		SpecName: "18mm 杉木生态板", PrintedAt: "2026-09-01T09:00:00+08:00",
		Codes: []string{"L-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterPrintBatch(ctx, PrintBatchInput{
		BatchID: "PB-2", PrinterRef: "PRESS-1", ProductRef: "SPEC-PLY-18",
		SpecName: "18mm 杉木生态板", PrintedAt: "2026-09-05T09:00:00+08:00",
		Codes: []string{"L-1"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("编号跨批复用应冲突，得到 %v", err)
	}
}
