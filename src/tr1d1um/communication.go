/**
 * Copyright 2017 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package main

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/Comcast/webpa-common/logging"
	"github.com/Comcast/webpa-common/wrp"
	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
)

//SendAndHandle wraps the methods to communicate both back to a requester and to a target server
type SendAndHandle interface {
	Send(*ConversionHandler, http.ResponseWriter, []byte, *http.Request) (*http.Response, error)
	HandleResponse(*ConversionHandler, error, *http.Response, http.ResponseWriter, bool)
	GetRespTimeout() time.Duration
}

//Tr1SendAndHandle implements the behaviors of SendAndHandle
type Tr1SendAndHandle struct {
	log            log.Logger
	NewHTTPRequest func(string, string, io.Reader) (*http.Request, error)
	respTimeout    time.Duration
}

type clientResponse struct {
	resp *http.Response
	err  error
}

//Send prepares and subsequently sends a WRP encoded message to a predefined server
//Its response is then handled in HandleResponse
func (tr1 *Tr1SendAndHandle) Send(ch *ConversionHandler, resp http.ResponseWriter, data []byte, req *http.Request) (respFromServer *http.Response, err error) {
	var errorLogger = logging.Error(tr1.log)
	wrpMsg := ch.wdmpConvert.GetConfiguredWRP(data, mux.Vars(req), req.Header)

	wrpPayload, err := ch.encodingHelper.GenericEncode(wrpMsg, wrp.JSON)

	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		errorLogger.Log(logging.ErrorKey(), err)
		return
	}

	fullPath := ch.targetURL + baseURI + "/" + ch.serverVersion + "/device"
	requestToServer, err := tr1.NewHTTPRequest(http.MethodPost, fullPath, bytes.NewBuffer(wrpPayload))

	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		errorLogger.Log(logging.ErrorKey(), err)
		return
	}

	requestToServer.Header.Set("Content-Type", wrp.JSON.ContentType())
	requestToServer.Header.Set("Authorization", req.Header.Get("Authorization"))

	respFromServer, err = ch.PerformRequest(requestToServer.WithContext(req.Context())) //keep ancestor's context
	return
}

//HandleResponse contains the instructions of what to write back to the original requester (origin)
//based on the responses of a server we have contacted through Send
func (tr1 *Tr1SendAndHandle) HandleResponse(ch *ConversionHandler, err error, respFromServer *http.Response, origin http.ResponseWriter,
	wholeBody bool) {
	var errorLogger = logging.Error(tr1.log)

	ForwardHeadersByPrefix("X", origin, respFromServer)

	if err != nil {
		ReportError(err, origin)
		errorLogger.Log(logging.ErrorKey(), err)
		return
	}

	if respFromServer.StatusCode != http.StatusOK {
		var bodyResp []byte
		if responseBody, err := ioutil.ReadAll(respFromServer.Body); err == nil {
			bodyResp = responseBody
		}

		origin.WriteHeader(respFromServer.StatusCode)
		origin.Write(bodyResp)
		errorLogger.Log(logging.MessageKey(), "non-200 response from server", logging.ErrorKey(), respFromServer.Status)
		return
	}

	if wholeBody { // Do not attempt extracting payload, forward whole body
		err = forwardInput(origin, respFromServer.Body)
		ReportError(err, origin)
		return
	}

	if RDKResponse, err := ch.encodingHelper.ExtractPayload(respFromServer.Body, wrp.JSON); err == nil {
		if RDKRespCode, err := GetStatusCodeFromRDKResponse(RDKResponse); err == nil && RDKRespCode != http.StatusInternalServerError {
			origin.WriteHeader(RDKRespCode)
		}
		origin.Write(RDKResponse)
	} else {
		ReportError(err, origin)
		errorLogger.Log(logging.ErrorKey(), err)
	}
}

//GetRespTimeout returns the duration the sender should use while waiting
//for a response from a server
func (tr1 *Tr1SendAndHandle) GetRespTimeout() time.Duration {
	return tr1.respTimeout
}

//Requester has the main functionality of taking a request, performing such request based on some internal client and
// simply returning the response and potential err when applicable
type Requester interface {
	PerformRequest(*http.Request) (*http.Response, error)
}

//ContextTimeoutRequester is a Requester realization that executes an http request respecting any context deadlines (or
// cancellations)
type ContextTimeoutRequester struct {
	client *http.Client
}

//PerformRequest makes its client execute the request asynchronously and guarantees that the cancellations or
// timeouts of the request's context is respected
func (c *ContextTimeoutRequester) PerformRequest(request *http.Request) (resp *http.Response, err error) {
	responseReady := make(chan clientResponse)
	ctx := request.Context()

	go func() {
		defer close(responseReady)
		respObj, respErr := c.client.Do(request)
		responseReady <- clientResponse{respObj, respErr}
	}()

	select {
	case cResponse := <-responseReady:
		resp, err = cResponse.resp, cResponse.err
		return
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

//forwardInput reads the given input into bytes and writes it to a given response writer
func forwardInput(origin http.ResponseWriter, input io.Reader) (err error) {
	if origin != nil && input != nil {
		if body, readErr := ioutil.ReadAll(input); readErr == nil {
			origin.Write(body)
		} else {
			err = readErr
		}
	}
	return
}