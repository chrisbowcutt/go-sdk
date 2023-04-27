package keploy

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func Test_GrpCurl(t *testing.T) {
	jsonString := `{"product_id":{"value":3485},"start_on":{"seconds":1683176400},"stop_on":{"seconds":1683178200},"synchronous":{"value":true}}`
	//bytes := []byte(jsonString)
	//jsonpb.Unmarshal(bytes)
	err := GrpCurl(jsonString, "tid:test-10", "localhost:50052", "parkmobile.resinventory.api.ResInventoryService.ProductAvailability")
	assert.Nil(t, err)
}
