package utils

import (
	"encoding/json"
	"github.com/golang/protobuf/proto"
	"io"
)



type CustomJsonRequestParser struct {
	dec *json.Decoder
	requestCount int
}

func NewCustomJsonRequestParser(in io.Reader) *CustomJsonRequestParser {
	return &CustomJsonRequestParser{
		dec: json.NewDecoder(in),
	}
}

func (f *CustomJsonRequestParser) Next(m proto.Message) error {
	var msg json.RawMessage
	if err := f.dec.Decode(&msg); err != nil {
		return err
	}
	f.requestCount++

	return proto.Unmarshal(msg, m)
	return nil
}

func (f *CustomJsonRequestParser) NumRequests() int {
	return f.requestCount
}
