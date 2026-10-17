package wechatpay

import (
	"crypto/rsa"
	"strconv"
	"time"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

// jsapiSignType is the signature algorithm WeChat's JSAPI invocation expects.
const jsapiSignType = "RSA"

// buildJSAPIParameters produces the signed parameter set the WeChat browser
// hands to WeixinJSBridge. The signature is RSA-SHA256 over the exact string
//
//	appId\ntimeStamp\nnonceStr\npackage\n
//
// signed with the merchant private key, per WeChat Pay's JSAPI specification.
func buildJSAPIParameters(appID, prepayID string, privateKey *rsa.PrivateKey, now time.Time) (*types.WechatPayJSAPIParameters, error) {
	nonce, err := utils.GenerateNonce()
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("Could not generate the JSAPI invocation nonce").
			Mark(ierr.ErrInternal)
	}
	timeStamp := strconv.FormatInt(now.Unix(), 10)
	pkg := "prepay_id=" + prepayID
	message := appID + "\n" + timeStamp + "\n" + nonce + "\n" + pkg + "\n"

	paySign, err := utils.SignSHA256WithRSA(message, privateKey)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("Could not sign the JSAPI invocation parameters with the merchant private key").
			Mark(ierr.ErrInternal)
	}

	return &types.WechatPayJSAPIParameters{
		AppID:     appID,
		TimeStamp: timeStamp,
		NonceStr:  nonce,
		Package:   pkg,
		SignType:  jsapiSignType,
		PaySign:   paySign,
	}, nil
}
