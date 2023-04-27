package khttpclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"sync"

	// "github.com/keploy/go-sdk/pkg"
	internal "github.com/keploy/go-sdk/pkg/keploy"

	"github.com/keploy/go-sdk/keploy"
	"github.com/keploy/go-sdk/mock"
	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

// ReadCloser is used so that gob could encode-decode http.Response.
type ReadCloser struct {
	*bytes.Reader
	Body io.ReadCloser
}

func (rc ReadCloser) Close() error {
	return nil
}

func (rc *ReadCloser) UnmarshalBinary(b []byte) error {

	// copy the byte array elements into copyByteArr. See https://www.reddit.com/r/golang/comments/tddjdd/gob_is_appending_gibberish_to_my_object/
	copyByteArr := make([]byte, len(b))
	copy(copyByteArr, b)
	rc.Reader = bytes.NewReader(copyByteArr)
	return nil
}

func (rc *ReadCloser) MarshalBinary() ([]byte, error) {
	if rc.Body != nil {
		b, err := ioutil.ReadAll(rc.Body)
		rc.Body.Close()
		rc.Reader = bytes.NewReader(b)
		return b, err
	}
	return nil, nil
}

type Interceptor struct {
	core http.RoundTripper
	log  *zap.Logger
	kctx *internal.Context
}

// NewInterceptor constructs and returns the pointer to Interceptor. Interceptor is used
// to intercept every http client calls and store their responses into keploy context.
// The default mode of the pkg keploy context of the interceptor returned here is MODE_OFF.
func NewInterceptor(core http.RoundTripper) *Interceptor {
	// Initialize a logger
	logger, _ := zap.NewProduction()
	defer func() {
		_ = logger.Sync() // flushes buffer, if any
	}()

	// Register types to gob encoder
	gob.Register(ReadCloser{})
	gob.Register(elliptic.P256())
	gob.Register(ecdsa.PublicKey{})
	gob.Register(rsa.PublicKey{})
	return &Interceptor{
		core: core,
		log:  logger,
		kctx: &internal.Context{
			Mode: internal.MODE_OFF,
			Mu:   &sync.Mutex{},
		},
	}
}

// SetContext is used to store the keploy context from request context into the Interceptor
// kctx field.
func (i *Interceptor) SetContext(requestContext context.Context) {
	// ctx := context.TODO()
	if kctx, err := internal.GetState(requestContext); err == nil {
		i.kctx = kctx
		i.log.Debug("http client keploy interceptor's context has been set to : ", zap.Any("keploy.Context ", i.kctx))
	}
}

// setRequestContext returns the context with keploy context as value. It is called only
// when kctx field of Interceptor is not null.
func (i *Interceptor) setRequestContext(ctx context.Context) context.Context {
	rctx := context.WithValue(ctx, internal.KCTX, i.kctx)
	return rctx
}

// RoundTrip is the custom method which is called before making http client calls to
// capture or replay the outputs of external http service.
func (i Interceptor) RoundTrip(r *http.Request) (*http.Response, error) {
	// If the request has no context with keploy context, we check the global keploy context
	// of the interceptor. If the interceptor context is set to MODE_OFF, we transparently
	// pass the request. If the request has a keploy context, we check the mode of the context
	// and if it is MODE_OFF, we transparently pass the request.
	if _, err := internal.GetState(r.Context()); err != nil {
		if i.kctx != nil && i.kctx.Mode == internal.MODE_OFF {
			return i.core.RoundTrip(r)
		}
	} else {
		if internal.GetModeFromContext(r.Context()) == internal.MODE_OFF {
			return i.core.RoundTrip(r)
		}
	}

	// Read the request body to store in meta
	var reqBody []byte
	if r.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(r.Body)
		if err != nil {
			// TODO right way to log errors
			i.log.Error("Unable to read request body", zap.Error(err))
			return nil, err
		}
	}
	if r.Body != http.NoBody {
		r.Body = io.NopCloser(bytes.NewBuffer(reqBody)) // Reset
	}

	// adds the keploy context stored in Interceptor's ctx field into the http client request context.
	if _, err := internal.GetState(r.Context()); err != nil && i.kctx != nil {
		ctx := i.setRequestContext(r.Context())
		r = r.WithContext(ctx)
	}

	var (
		err       error
		kerr      *keploy.KError = &keploy.KError{}
		resp      *http.Response = &http.Response{}
		isRespNil bool           = false
	)
	kctx, er := internal.GetState(r.Context())
	if er != nil {
		return nil, er
	}
	mode := kctx.Mode
	meta := map[string]string{
		"name":      "Http",
		"type":      string(models.HttpClient),
		"operation": r.Method,
	}
	switch mode {
	case internal.MODE_TEST:
		//don't call i.core.RoundTrip method when not in file export
		resp1, err1, ok := MockRespFromYaml(kctx, i.log, r, reqBody, meta)
		if ok {
			return resp1, err1
		}
	case internal.MODE_RECORD:
		resp, err = i.core.RoundTrip(r)
		var (
			respBody   []byte
			statusCode int
			respHeader http.Header
			errStr     string = ""
		)
		if resp != nil {
			// Read the response body to capture
			if resp.Body != nil { // Read
				var err error
				respBody, err = ioutil.ReadAll(resp.Body)
				if err != nil {
					i.log.Error("Unable to read request body", zap.Error(err))
					return nil, err
				}
			}
			resp.Body = ioutil.NopCloser(bytes.NewBuffer(respBody)) // Reset
			statusCode = resp.StatusCode
			respHeader = resp.Header
		}

		if err != nil {
			errStr = err.Error()
		}
		httpMock := &proto.Mock{
			Version: string(models.V1Beta2),
			Name:    kctx.TestID,
			Kind:    string(models.HTTP),
			Spec: &proto.Mock_SpecSchema{
				Metadata: meta,
				Objects: []*proto.Mock_Object{
					{
						Type: "error",
						Data: []byte(errStr),
					},
				},
				Req: &proto.HttpReq{
					Method:     r.Method,
					ProtoMajor: int64(r.ProtoMajor),
					ProtoMinor: int64(r.ProtoMinor),
					URL:        r.URL.String(),
					Header:     mock.GetProtoMap(r.Header),
					// Body:       string(reqBody),
					BodyData: reqBody,
				},
				Res: &proto.HttpResp{
					StatusCode: int64(statusCode),
					Header:     mock.GetProtoMap(respHeader),
					// Body:       string(respBody),
					BodyData: respBody,
				},
			},
		}
		if internal.GetGrpcClient() != nil && kctx.FileExport && internal.MockId.Unique(kctx.TestID) {
			recorded := internal.PutMock(r.Context(), internal.MockPath, httpMock)
			if recorded {
				fmt.Println("🟠 Captured the mocked outputs for Http dependency call with meta: ", meta)
			}
			return resp, err
		}
		kctx.Mock = append(kctx.Mock, httpMock)

		if resp == nil {
			isRespNil = true
			resp = &http.Response{}
		}
		kerr, resp = toGobType(err, resp)
		outputs := []interface{}{resp, kerr}
		res := make([][]byte, len(outputs))
		for indx, t := range outputs {
			err = keploy.Encode(t, res, indx)
			if err != nil {
				i.log.Error("dependency capture failed: failed to encode object", zap.String("type", reflect.TypeOf(t).String()), zap.String("test id", kctx.TestID), zap.Error(err))
			}
		}
		kctx.Deps = append(kctx.Deps, models.Dependency{
			Name: meta["name"],
			Type: models.DependencyType(meta["type"]),
			Data: res,
			Meta: meta,
		})
		if isRespNil {
			return nil, err
		}

		return resp, err
	default:
		return nil, errors.New("integrations: Not in a valid sdk mode")
	}

	kerr, resp = toGobType(err, resp)

	mock, res := keploy.ProcessDep(r.Context(), i.log, meta, resp, kerr)
	if mock {
		var mockErr error
		x := res[1].(*keploy.KError)
		if x.Err != nil {
			mockErr = x.Err
		}
		return resp, mockErr
	}
	if isRespNil {
		return nil, err
	}
	return resp, err

}