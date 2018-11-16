package translation

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"tr1d1um/common"

	money "github.com/Comcast/golang-money"
	"github.com/Comcast/webpa-common/wrp"
	"github.com/justinas/alice"

	kitlog "github.com/go-kit/kit/log"
	kithttp "github.com/go-kit/kit/transport/http"

	"github.com/gorilla/mux"
)

const (
	applicationName, apiBase = "tr1d1um", "/api/v2"
	contentTypeHeaderKey     = "Content-Type"
	authHeaderKey            = "Authorization"
)

type xmidtResponse struct {
	Body             []byte
	ForwardedHeaders http.Header
	Code             int
}

//Options wraps the properties needed to set up the translation server
type Options struct {
	S Service

	//APIRouter is assumed to be a subrouter with the API prefix path (i.e. 'api/v2')
	APIRouter *mux.Router

	Authenticate  *alice.Chain
	Log           kitlog.Logger
	ValidServices []string
}

//ConfigHandler sets up the server that powers the translation service
func ConfigHandler(c *Options) {
	opts := []kithttp.ServerOption{
		kithttp.ServerBefore(common.Capture),
		kithttp.ServerErrorEncoder(common.ErrorLogEncoder(c.Log, encodeError)),
		kithttp.ServerFinalizer(common.TransactionLogging(c.Log)),
	}

	WRPHandler := kithttp.NewServer(
		makeTranslationEndpoint(c.S),
		decodeValidServiceRequest(c.ValidServices, decodeRequest),
		encodeResponse,
		opts...,
	)

	//TODO: TMP IOT HACK
	c.APIRouter.Handle("/device/{deviceid}/{service:iot}", c.Authenticate.Then(common.Welcome(WRPHandler))).
		Methods(http.MethodPost)

	c.APIRouter.Handle("/device/{deviceid}/{service}", c.Authenticate.Then(common.Welcome(WRPHandler))).
		Methods(http.MethodGet, http.MethodPatch)

	c.APIRouter.Handle("/device/{deviceid}/{service}/{parameter}", c.Authenticate.Then(common.Welcome(WRPHandler))).
		Methods(http.MethodDelete, http.MethodPut, http.MethodPost)
}

/* Request Decoding */

func decodeRequest(ctx context.Context, r *http.Request) (decodedRequest interface{}, err error) {
	if ok, err := money.CheckHeaderForMoneyTrace(r.Header); err == nil {
		tc := money.DecodeTraceContext(r.Header)
		httpspanner := money.NewHTTPSpanner(money.StarterON())
		ht := httpspanner.Start(request.Context(), money.NewSpan(tc))
		decodeRequest = &wrpRequest{httpTracker: ht}
	}

	var (
		payload []byte
		wrpMsg  *wrp.Message
	)

	if payload, err = requestPayload(r); err == nil {
		var tid = ctx.Value(common.ContextKeyRequestTID).(string)
		if wrpMsg, err = wrap(payload, tid, mux.Vars(r)); err == nil {
			decodedRequest = &wrpRequest{
				WRPMessage:      wrpMsg,
				AuthHeaderValue: r.Header.Get(authHeaderKey),
				httpTracker:     ht,
			}
		}
	}

	return
}

func requestPayload(r *http.Request) (payload []byte, err error) {

	switch r.Method {
	case http.MethodGet:
		payload, err = requestGetPayload(r.FormValue("names"), r.FormValue("attributes"))
	case http.MethodPatch:
		payload, err = requestSetPayload(r.Body, r.Header.Get(HeaderWPASyncNewCID), r.Header.Get(HeaderWPASyncOldCID), r.Header.Get(HeaderWPASyncCMC))
	case http.MethodDelete:
		payload, err = requestDeletePayload(mux.Vars(r))
	case http.MethodPut:
		payload, err = requestReplacePayload(mux.Vars(r), r.Body)
	case http.MethodPost:

		/****TODO: TMP IOT ENDPOINT HACK****/
		v := mux.Vars(r)
		if v["service"] == "iot" {
			if v["parameters"] == "" {
				return ioutil.ReadAll(r.Body)
			}
			//TODO: this might also be doable at the mux level
			// /iot endpoint should not pass anything through queries (all request data is in the body)
			return nil, ErrInvalidService
		}
		/********/

		payload, err = requestAddPayload(v, r.Body)

	default:
		//Unwanted methods should be filtered at the mux level. Thus, we "should" never get here
		err = ErrUnsupportedMethod
	}

	return
}

/* Response Encoding */

func encodeResponse(ctx context.Context, w http.ResponseWriter, response interface{}) (err error) {
	var resp = response.(*common.XmidtResponse)

	//equivalent to forwarding all headers
	common.ForwardHeadersByPrefix("", resp.ForwardedHeaders, w.Header())

	// Write TransactionID for all requests
	w.Header().Set(common.HeaderWPATID, ctx.Value(common.ContextKeyRequestTID).(string))

	if resp.Code != http.StatusOK { //just forward the XMiDT cluster response
		w.WriteHeader(resp.Code)
		_, err = w.Write(resp.Body)
		return
	}

	wrpModel := new(wrp.Message)

	if err = wrp.NewDecoderBytes(resp.Body, wrp.Msgpack).Decode(wrpModel); err == nil {

		var deviceResponseModel struct {
			StatusCode int `json:"statusCode"`
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		// if possible, use the device response status code
		if errUnmarshall := json.Unmarshal(wrpModel.Payload, &deviceResponseModel); errUnmarshall == nil {
			if deviceResponseModel.StatusCode != 0 && deviceResponseModel.StatusCode != http.StatusInternalServerError {
				w.WriteHeader(deviceResponseModel.StatusCode)
			}
		}

		_, err = w.Write(wrpModel.Payload)
	}

	tracker, ok := money.TrackerFromContext(ctx)
	if ok {
		result, _ := tracker.Finish()
		w = money.WriteSpansHeaderTr1d1um(result, w, response)
	}

	return
}

/* Error Encoding */

func encodeError(ctx context.Context, err error, w http.ResponseWriter) {
	w.Header().Set(contentTypeHeaderKey, "application/json; charset=utf-8")
	w.Header().Set(common.HeaderWPATID, ctx.Value(common.ContextKeyRequestTID).(string))

	if ce, ok := err.(common.CodedError); ok {
		w.WriteHeader(ce.StatusCode())
	} else {
		w.WriteHeader(http.StatusInternalServerError)

		//the real error is logged into our system before encodeError() is called
		//the idea behind masking it is to not send the external API consumer internal error messages
		err = common.ErrTr1d1umInternal
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": err.Error(),
	})

}

/* Request-type specific decoding functions */

func requestSetPayload(in io.Reader, newCID, oldCID, syncCMC string) (p []byte, err error) {
	var (
		wdmp = new(setWDMP)
		data []byte
	)

	if data, err = ioutil.ReadAll(in); err == nil {

		//read data into wdmp
		if err = json.Unmarshal(data, wdmp); err == nil || len(data) == 0 { //len(data) == 0 case is for TEST_SET
			if err = deduceSET(wdmp, newCID, oldCID, syncCMC); err == nil {
				if !isValidSetWDMP(wdmp) {
					return nil, ErrInvalidSetWDMP
				}
				return json.Marshal(wdmp)
			}
		}
	}

	return
}

func requestGetPayload(names, attributes string) ([]byte, error) {
	if names == "" {
		return nil, ErrEmptyNames
	}

	wdmp := new(getWDMP)

	//default values at this point
	wdmp.Names, wdmp.Command = strings.Split(names, ","), CommandGet

	if attributes != "" {
		wdmp.Command, wdmp.Attributes = CommandGetAttrs, attributes
	}

	return json.Marshal(wdmp)
}

func requestAddPayload(m map[string]string, input io.Reader) (p []byte, err error) {
	var wdmp = &addRowWDMP{Command: CommandAddRow}

	if table, ok := m["parameter"]; ok {
		wdmp.Table = table
	} else {
		return nil, ErrMissingTable
	}

	var payload []byte
	if payload, err = ioutil.ReadAll(input); err == nil {
		if len(payload) == 0 {
			return nil, ErrMissingRow
		}

		if err = json.Unmarshal(payload, &wdmp.Row); err == nil {
			return json.Marshal(wdmp)
		}
	}

	return
}

func requestReplacePayload(m map[string]string, input io.Reader) (p []byte, err error) {
	var wdmp = &replaceRowsWDMP{Command: CommandReplaceRows}

	if table, ok := m["parameter"]; ok {
		wdmp.Table = table
	} else {
		return nil, ErrMissingTable
	}

	var payload []byte
	if payload, err = ioutil.ReadAll(input); err == nil {
		if len(payload) == 0 {
			return nil, ErrMissingRows
		}

		if err = json.Unmarshal(payload, &wdmp.Rows); err == nil {
			return json.Marshal(wdmp)
		}
	}

	return
}

func requestDeletePayload(m map[string]string) ([]byte, error) {
	if row, ok := m["parameter"]; ok {
		return json.Marshal(&deleteRowDMP{Command: CommandDeleteRow, Row: row})
	}
	return nil, ErrMissingRow
}
