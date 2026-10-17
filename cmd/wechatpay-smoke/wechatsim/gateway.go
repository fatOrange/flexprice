package wechatsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequestRecord captures one signed request the production SDK sent to the
// simulated gateway, so a test can assert on what the client actually requested.
type RequestRecord struct {
	Method            string
	Path              string
	RawQuery          string
	Body              []byte
	MchID             string
	SerialNo          string
	SignatureVerified bool
	// VerifyError carries why signature verification failed, when it did, so a
	// failing run can report the exact canonicalization problem instead of only
	// the provider's opaque 401.
	VerifyError string
	ReceivedAt  time.Time
}

// OrderState is the simulated gateway's view of one out_trade_no.
type OrderState struct {
	OutTradeNo    string
	MchID         string
	AppID         string
	AmountFen     int64
	Currency      string
	TradeType     string // NATIVE | JSAPI
	TradeState    string // NOTPAY | SUCCESS | CLOSED
	TransactionID string
	SuccessTime   string
	PayerOpenID   string
	NotifyURL     string
	TimeExpire    time.Time
	CreatedAt     time.Time
	NativeCodeURL string
	PrepayID      string
}

// RefundState is the simulated gateway's view of one out_refund_no.
type RefundState struct {
	OutRefundNo   string
	RefundID      string
	OutTradeNo    string
	TransactionID string
	Status        string // PROCESSING | SUCCESS
	RefundFen     int64
	TotalFen      int64
	Currency      string
	SuccessTime   string
	CreatedAt     time.Time
}

type transactionAmount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

// TransactionParams renders a WeChat Pay transaction object (the shape carried in
// query responses and inside encrypted notifications).
type TransactionParams struct {
	AppID         string
	MchID         string
	OutTradeNo    string
	TransactionID string
	TradeState    string
	TradeType     string
	SuccessTime   string
	OpenID        string
	TotalFen      int64
	Currency      string
	// OmitAmount renders a transaction without the amount block, which the
	// official schema permits for non-paid states and which the provider must
	// accept without treating the missing block as a settlement signal.
	OmitAmount bool
}

func TransactionJSON(p TransactionParams) []byte {
	obj := map[string]any{
		"appid":          p.AppID,
		"mchid":          p.MchID,
		"out_trade_no":   p.OutTradeNo,
		"transaction_id": p.TransactionID,
		"trade_type":     p.TradeType,
		"trade_state":    p.TradeState,
	}
	if !p.OmitAmount {
		obj["amount"] = transactionAmount{
			Total:    p.TotalFen,
			Currency: p.Currency,
		}
	}
	if p.SuccessTime != "" {
		obj["success_time"] = p.SuccessTime
	}
	if p.OpenID != "" {
		obj["payer"] = map[string]string{"openid": p.OpenID}
	}
	body, _ := json.Marshal(obj)
	return body
}

// RefundParams renders a WeChat Pay refund object.
type RefundParams struct {
	OutTradeNo    string
	TransactionID string
	OutRefundNo   string
	RefundID      string
	Status        string
	SuccessTime   string
	RefundFen     int64
	TotalFen      int64
	Currency      string
}

func RefundJSON(p RefundParams) []byte {
	obj := map[string]any{
		"out_trade_no":   p.OutTradeNo,
		"transaction_id": p.TransactionID,
		"out_refund_no":  p.OutRefundNo,
		"refund_id":      p.RefundID,
		"status":         p.Status,
		"amount": map[string]any{
			"total":    p.TotalFen,
			"refund":   p.RefundFen,
			"currency": p.Currency,
		},
	}
	if p.SuccessTime != "" {
		obj["success_time"] = p.SuccessTime
	}
	body, _ := json.Marshal(obj)
	return body
}

// Server is the isolated HTTP sink that stands in for api.mch.weixin.qq.com.
// Production code reaches it only through an injected *http.Client, so the
// real SDK still signs, verifies, and decrypts everything.
type Server struct {
	fixture *Fixture

	srv *httptest.Server

	mu          sync.Mutex
	orders      map[string]*OrderState
	refunds     map[string]*RefundState
	requests    []RequestRecord
	refundSeq   int
	requestSeq  int
	closeCalls  map[string]int
	queryCalls  map[string]int
	refundCalls []RequestRecord

	queryOverrides map[string]QueryOverride
	forceClosePaid map[string]bool
}

// QueryOverride stages a provider transaction-query response that disagrees
// with the stored order, so tests can prove the client checks identity before
// trusting state claims. Nil fields keep the stored value; SkipQueries leaves
// the first N queries for the order honest so a close re-query can be made to
// disagree after an initial truthful response.
type QueryOverride struct {
	SkipQueries   int
	AppID         *string
	MchID         *string
	OutTradeNo    *string
	TransactionID *string
	TradeState    *string
	OmitAmount    *bool
}

func (o QueryOverride) apply(order *OrderState) {
	if o.AppID != nil {
		order.AppID = *o.AppID
	}
	if o.MchID != nil {
		order.MchID = *o.MchID
	}
	if o.OutTradeNo != nil {
		order.OutTradeNo = *o.OutTradeNo
	}
	if o.TransactionID != nil {
		order.TransactionID = *o.TransactionID
	}
	if o.TradeState != nil {
		order.TradeState = *o.TradeState
	}
}

// NewServer builds the simulator around a fixture; call Start to listen.
func NewServer(f *Fixture) *Server {
	s := &Server{
		fixture:        f,
		orders:         make(map[string]*OrderState),
		refunds:        make(map[string]*RefundState),
		closeCalls:     make(map[string]int),
		queryCalls:     make(map[string]int),
		queryOverrides: make(map[string]QueryOverride),
		forceClosePaid: make(map[string]bool),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// Close shuts the local listener down.
func (s *Server) Close() { s.srv.Close() }

// URL is the loopback base URL of the simulator.
func (s *Server) URL() string { return s.srv.URL }

// HTTPClient returns a client that rewrites every request to the simulator
// while preserving the signed canonical request URI (path + query), exactly as
// the production SDK computed it. This is the injected transport the gateway
// constructor accepts; there is no production base-URL override.
func (s *Server) HTTPClient() *http.Client {
	target := strings.TrimPrefix(s.srv.URL, "http://")
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &rewriteTransport{
			target: target,
			base:   http.DefaultTransport,
		},
	}
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = t.target
	clone.URL = &u
	return t.base.RoundTrip(clone)
}

// Requests returns a copy of every request seen (successful or not).
func (s *Server) Requests() []RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestRecord, len(s.requests))
	copy(out, s.requests)
	return out
}

// RefundRequests returns only refund-create/query requests.
func (s *Server) RefundRequests() []RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestRecord, len(s.refundCalls))
	copy(out, s.refundCalls)
	return out
}

// Order returns the stored state for an out_trade_no (nil when unknown).
func (s *Server) Order(outTradeNo string) *OrderState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.orders[outTradeNo]; ok {
		cp := *o
		return &cp
	}
	return nil
}

// Refund returns the stored state for an out_refund_no.
func (s *Server) Refund(outRefundNo string) *RefundState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.refunds[outRefundNo]; ok {
		cp := *r
		return &cp
	}
	return nil
}

// MarkPaid drives the simulated payer to settle an order. successTime is the
// payer's success moment (may precede the local expiry check in races).
func (s *Server) MarkPaid(outTradeNo string, successTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[outTradeNo]
	if !ok {
		return
	}
	o.TradeState = "SUCCESS"
	o.TransactionID = fmt.Sprintf("42000000FAKE%08d", len(s.orders))
	o.SuccessTime = successTime.Format("2006-01-02T15:04:05+08:00")
}

// MarkRefundSuccess flips a previously accepted refund to SUCCESS, the state a
// late refund query or callback observes.
func (s *Server) MarkRefundSuccess(outRefundNo string) {
	s.markRefundStatus(outRefundNo, "SUCCESS")
}

// MarkRefundAbnormal flips a refund to ABNORMAL: the provider could not pay it
// out and it needs manual handling at the merchant platform.
func (s *Server) MarkRefundAbnormal(outRefundNo string) {
	s.markRefundStatus(outRefundNo, "ABNORMAL")
}

// MarkRefundClosed flips a refund to CLOSED: the provider terminated it.
func (s *Server) MarkRefundClosed(outRefundNo string) {
	s.markRefundStatus(outRefundNo, "CLOSED")
}

func (s *Server) markRefundStatus(outRefundNo, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refunds[outRefundNo]
	if !ok {
		return
	}
	r.Status = status
	if status == "SUCCESS" {
		r.SuccessTime = time.Now().Format("2006-01-02T15:04:05+08:00")
	}
}

// CloseCalls reports how many close attempts the simulator saw for an order.
func (s *Server) CloseCalls(outTradeNo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls[outTradeNo]
}

// SetQueryOverride stages what the next transaction queries for an order return.
func (s *Server) SetQueryOverride(outTradeNo string, override QueryOverride) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queryOverrides[outTradeNo] = override
}

// ForceClosePaidError makes CLOSE for an order fail with WeChat's ORDERPAID,
// modelling the race where the payer settles between the query and the close.
func (s *Server) ForceClosePaidError(outTradeNo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceClosePaid[outTradeNo] = true
}

// QueryCalls reports how many transaction queries the simulator saw.
func (s *Server) QueryCalls(outTradeNo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryCalls[outTradeNo]
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "cannot read body")
		return
	}

	record := RequestRecord{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		Body:       body,
		ReceivedAt: time.Now().UTC(),
	}

	authz := r.Header.Get("Authorization")
	verified := false
	verifyErr := ""
	switch {
	case authz == "":
		verifyErr = "authorization header is missing"
	default:
		parts, parseErr := ParseAuthorizationHeader(authz)
		switch {
		case parseErr != nil:
			verifyErr = parseErr.Error()
		default:
			record.MchID = parts.MchID
			record.SerialNo = parts.SerialNo
			message := RequestSignatureMessage(r.Method, r.URL.RequestURI(), parts.Timestamp, parts.Nonce, string(body))
			// Wechatpay-Serial on the request is the SDK's accept hint and is
			// deliberately not part of request authentication; the signature
			// over the canonical string is the proof.
			switch {
			case VerifySHA256WithRSA(message, parts.Signature, s.fixture.MerchantPublicKey()) != nil:
				verifyErr = "request signature does not verify against the merchant key"
			case parts.MchID != s.fixture.MerchantID:
				verifyErr = fmt.Sprintf("unexpected mchid %s", parts.MchID)
			default:
				verified = true
			}
		}
	}
	record.SignatureVerified = verified
	record.VerifyError = verifyErr

	s.mu.Lock()
	s.requestSeq++
	s.requests = append(s.requests, record)
	s.mu.Unlock()

	if !verified {
		s.writeError(w, http.StatusUnauthorized, "SIGN_ERROR", "request verification failed: "+verifyErr)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v3/pay/transactions/native":
		s.handlePrepay(w, body, "NATIVE")
	case r.Method == http.MethodPost && r.URL.Path == "/v3/pay/transactions/jsapi":
		s.handlePrepay(w, body, "JSAPI")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v3/pay/transactions/out-trade-no/"):
		s.handleQueryByOrder(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v3/pay/transactions/id/"):
		s.handleQueryByID(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v3/pay/transactions/out-trade-no/") && strings.HasSuffix(r.URL.Path, "/close"):
		s.handleClose(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v3/refund/domestic/refunds":
		s.handleRefundCreate(w, body)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v3/refund/domestic/refunds/"):
		s.handleRefundQuery(w, r)
	default:
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown simulated endpoint "+r.URL.Path)
	}
}

type prepayRequest struct {
	AppID       string `json:"appid"`
	MchID       string `json:"mchid"`
	Description string `json:"description"`
	OutTradeNo  string `json:"out_trade_no"`
	TimeExpire  string `json:"time_expire"`
	NotifyURL   string `json:"notify_url"`
	Attach      string `json:"attach"`
	Amount      struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Payer *struct {
		OpenID string `json:"openid"`
	} `json:"payer"`
}

func (s *Server) handlePrepay(w http.ResponseWriter, body []byte, tradeType string) {
	var req prepayRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "invalid prepay body")
		return
	}
	if req.MchID != s.fixture.MerchantID {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", fmt.Sprintf("mchid %q is not the simulated merchant", req.MchID))
		return
	}
	if req.AppID != s.fixture.AppID {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", fmt.Sprintf("appid %q is not the simulated app", req.AppID))
		return
	}
	if req.Amount.Total <= 0 {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "amount.total must be positive")
		return
	}
	if req.Amount.Currency != "CNY" {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "currency must be CNY")
		return
	}
	if tradeType == "JSAPI" && (req.Payer == nil || req.Payer.OpenID == "") {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "jsapi requires payer.openid")
		return
	}
	expireAt, err := time.Parse("2006-01-02T15:04:05-07:00", req.TimeExpire)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "time_expire must be RFC3339 with seconds")
		return
	}

	s.mu.Lock()
	seq := len(s.orders) + 1
	order := &OrderState{
		OutTradeNo:    req.OutTradeNo,
		MchID:         req.MchID,
		AppID:         req.AppID,
		AmountFen:     req.Amount.Total,
		Currency:      req.Amount.Currency,
		TradeType:     tradeType,
		TradeState:    "NOTPAY",
		TimeExpire:    expireAt,
		NotifyURL:     req.NotifyURL,
		CreatedAt:     time.Now(),
		NativeCodeURL: fmt.Sprintf("weixin://wxpay/bizpayurl?pr=FAKESIM%04d", seq),
		PrepayID:      fmt.Sprintf("wxFAKESIM%020d", seq),
	}
	if req.Payer != nil {
		order.PayerOpenID = req.Payer.OpenID
	}
	s.orders[req.OutTradeNo] = order
	s.mu.Unlock()

	if tradeType == "NATIVE" {
		s.writeJSON(w, http.StatusOK, map[string]string{"code_url": order.NativeCodeURL})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"prepay_id": order.PrepayID})
}

func (s *Server) handleQueryByOrder(w http.ResponseWriter, r *http.Request) {
	outTradeNo, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v3/pay/transactions/out-trade-no/"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "bad out_trade_no")
		return
	}
	if mchid := r.URL.Query().Get("mchid"); mchid != s.fixture.MerchantID {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "mchid query parameter mismatch")
		return
	}

	s.mu.Lock()
	s.queryCalls[outTradeNo]++
	queryNumber := s.queryCalls[outTradeNo]
	o, ok := s.orders[outTradeNo]
	var snap *OrderState
	if ok {
		cp := *o
		snap = &cp
	}
	override, hasOverride := s.queryOverrides[outTradeNo]
	s.mu.Unlock()

	if !ok {
		s.writeError(w, http.StatusNotFound, "ORDER_NOT_EXIST", "order not found: "+outTradeNo)
		return
	}
	if snap.TradeState == "CLOSED" {
		s.writeError(w, http.StatusBadRequest, "ORDER_CLOSED", "order closed")
		return
	}

	// A staged override renders the disagreement after the order-existence and
	// real-closed guards, so it can model a provider response claiming any
	// identity or state without changing what is actually stored.
	var omitAmount bool
	if hasOverride && override.SkipQueries < queryNumber {
		override.apply(snap)
		omitAmount = override.OmitAmount != nil && *override.OmitAmount
	}

	s.writeJSONBody(w, http.StatusOK, TransactionJSON(TransactionParams{
		AppID:         snap.AppID,
		MchID:         snap.MchID,
		OutTradeNo:    snap.OutTradeNo,
		TransactionID: snap.TransactionID,
		TradeState:    snap.TradeState,
		TradeType:     snap.TradeType,
		SuccessTime:   snap.SuccessTime,
		OpenID:        snap.PayerOpenID,
		TotalFen:      snap.AmountFen,
		Currency:      snap.Currency,
		OmitAmount:    omitAmount,
	}))
}

func (s *Server) handleQueryByID(w http.ResponseWriter, r *http.Request) {
	transactionID, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v3/pay/transactions/id/"))
	if err != nil || transactionID == "" {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "bad transaction id")
		return
	}
	s.mu.Lock()
	var found *OrderState
	for _, o := range s.orders {
		if o.TransactionID == transactionID {
			cp := *o
			found = &cp
			break
		}
	}
	s.mu.Unlock()
	if found == nil {
		s.writeError(w, http.StatusNotFound, "ORDER_NOT_EXIST", "transaction not found: "+transactionID)
		return
	}
	s.writeJSONBody(w, http.StatusOK, TransactionJSON(TransactionParams{
		AppID:         found.AppID,
		MchID:         found.MchID,
		OutTradeNo:    found.OutTradeNo,
		TransactionID: found.TransactionID,
		TradeState:    found.TradeState,
		TradeType:     found.TradeType,
		SuccessTime:   found.SuccessTime,
		OpenID:        found.PayerOpenID,
		TotalFen:      found.AmountFen,
		Currency:      found.Currency,
	}))
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v3/pay/transactions/out-trade-no/")
	outTradeNo, err := url.PathUnescape(strings.TrimSuffix(path, "/close"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "bad out_trade_no")
		return
	}
	s.mu.Lock()
	s.closeCalls[outTradeNo]++
	o, ok := s.orders[outTradeNo]
	var state string
	forcedPaid := s.forceClosePaid[outTradeNo]
	if ok {
		state = o.TradeState
	}
	s.mu.Unlock()

	if !ok {
		s.writeError(w, http.StatusNotFound, "ORDER_NOT_EXIST", "order not found: "+outTradeNo)
		return
	}
	if state == "SUCCESS" || forcedPaid {
		s.writeError(w, http.StatusBadRequest, "ORDERPAID", "order has been paid")
		return
	}
	if state == "NOTPAY" {
		s.mu.Lock()
		if o, ok := s.orders[outTradeNo]; ok {
			o.TradeState = "CLOSED"
		}
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	s.signAndWrite(w, http.StatusNoContent, nil)
}

type refundCreateRequest struct {
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	OutRefundNo   string `json:"out_refund_no"`
	Reason        string `json:"reason"`
	NotifyURL     string `json:"notify_url"`
	Amount        struct {
		Refund   int64  `json:"refund"`
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

func (s *Server) handleRefundCreate(w http.ResponseWriter, body []byte) {
	var req refundCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "invalid refund body")
		return
	}
	if (req.OutTradeNo == "" && req.TransactionID == "") || req.OutRefundNo == "" {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "out_trade_no or transaction_id, and out_refund_no, are required")
		return
	}
	if req.Amount.Currency != "CNY" || req.Amount.Refund <= 0 || req.Amount.Total <= 0 {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "refund amount must be positive CNY")
		return
	}

	s.mu.Lock()
	var order *OrderState
	if req.OutTradeNo != "" {
		order = s.orders[req.OutTradeNo]
	}
	if order == nil && req.TransactionID != "" {
		for _, candidate := range s.orders {
			if candidate.TransactionID == req.TransactionID {
				order = candidate
				break
			}
		}
	}
	if order == nil {
		s.mu.Unlock()
		s.writeError(w, http.StatusNotFound, "ORDER_NOT_EXIST", "order not found for refund")
		return
	}
	if order.TradeState != "SUCCESS" {
		s.mu.Unlock()
		s.writeError(w, http.StatusBadRequest, "ORDER_NOT_PAID", "order is not paid")
		return
	}
	if req.Amount.Total != order.AmountFen {
		s.mu.Unlock()
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", fmt.Sprintf("total %d does not match order total %d", req.Amount.Total, order.AmountFen))
		return
	}
	if existing, dup := s.refunds[req.OutRefundNo]; dup && existing.Status != "" {
		cp := *existing
		s.mu.Unlock()
		s.writeJSONBody(w, http.StatusOK, RefundJSON(refundParamsOf(&cp)))
		return
	}
	s.refundSeq++
	refund := &RefundState{
		OutRefundNo:   req.OutRefundNo,
		RefundID:      fmt.Sprintf("50000000FAKEREFUND%010d", s.refundSeq),
		OutTradeNo:    req.OutTradeNo,
		TransactionID: order.TransactionID,
		Status:        "PROCESSING",
		RefundFen:     req.Amount.Refund,
		TotalFen:      req.Amount.Total,
		Currency:      req.Amount.Currency,
		CreatedAt:     time.Now(),
	}
	s.refunds[req.OutRefundNo] = refund
	cp := *refund
	s.refundCalls = append(s.refundCalls, RequestRecord{Method: http.MethodPost, Path: "/v3/refund/domestic/refunds", Body: body, MchID: s.fixture.MerchantID, SignatureVerified: true})
	s.mu.Unlock()

	s.writeJSONBody(w, http.StatusOK, RefundJSON(refundParamsOf(&cp)))
}

func (s *Server) handleRefundQuery(w http.ResponseWriter, r *http.Request) {
	outRefundNo, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v3/refund/domestic/refunds/"))
	if err != nil || outRefundNo == "" {
		s.writeError(w, http.StatusBadRequest, "PARAM_ERROR", "bad out_refund_no")
		return
	}
	s.mu.Lock()
	refund, ok := s.refunds[outRefundNo]
	var cp *RefundState
	if ok {
		c := *refund
		cp = &c
	}
	if ok {
		s.refundCalls = append(s.refundCalls, RequestRecord{Method: http.MethodGet, Path: r.URL.Path, RawQuery: r.URL.RawQuery, MchID: s.fixture.MerchantID, SignatureVerified: true})
	}
	s.mu.Unlock()
	if !ok {
		// WeChat Pay reports a missing refund as 404 RESOURCE_NOT_EXISTS; the
		// protocol maps any verified 404 for refund queries to not-found.
		s.writeError(w, http.StatusNotFound, "RESOURCE_NOT_EXISTS", "refund not found: "+outRefundNo)
		return
	}
	s.writeJSONBody(w, http.StatusOK, RefundJSON(refundParamsOf(cp)))
}

func refundParamsOf(r *RefundState) RefundParams {
	return RefundParams{
		OutTradeNo:    r.OutTradeNo,
		TransactionID: r.TransactionID,
		OutRefundNo:   r.OutRefundNo,
		RefundID:      r.RefundID,
		Status:        r.Status,
		SuccessTime:   r.SuccessTime,
		RefundFen:     r.RefundFen,
		TotalFen:      r.TotalFen,
		Currency:      r.Currency,
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSONBody(w, status, mustJSON(map[string]string{"code": code, "message": message}))
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	s.writeJSONBody(w, status, mustJSON(payload))
}

func (s *Server) writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	s.signAndWrite(w, status, body)
}

// signAndWrite platform-signs every response, including errors and the empty
// 204 close response, exactly as WeChat Pay does. This must stay true: the
// production provider re-verifies the platform signature over every non-2xx
// body with the official response validator before trusting its error code, so
// an unsigned 404 ORDER_NOT_EXIST or 400 ORDERPAID would be treated as an
// uncertain provider failure instead of a definitive outcome.
func (s *Server) signAndWrite(w http.ResponseWriter, status int, body []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := randomHex(16)
	if err != nil {
		http.Error(w, "nonce failure", http.StatusInternalServerError)
		return
	}
	signature, err := SignSHA256WithRSA(ResponseSignatureMessage(timestamp, nonce, body), s.fixture.PlatformPrivateKey)
	if err != nil {
		http.Error(w, "sign failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Wechatpay-Timestamp", timestamp)
	w.Header().Set("Wechatpay-Nonce", nonce)
	w.Header().Set("Wechatpay-Signature", signature)
	w.Header().Set("Wechatpay-Serial", s.fixture.PlatformPublicKeyID)
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func mustJSON(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return body
}
