package rpc

import (
	"testing"
	"time"

	"gopkg.in/check.v1"

	"github.com/eleme/lindb/pkg/logger"
	"github.com/eleme/lindb/rpc"
	"github.com/eleme/lindb/rpc/proto/common"
)

const (
	bindAddress = ":9001"
	timeout     = time.Second
)

type brokerTestSuite struct {
	bs BrokerServer
}

var _ = check.Suite(&brokerTestSuite{
	bs: NewBrokerServer(bindAddress),
})

func Test(t *testing.T) {
	check.TestingT(t)
}

func (ts *brokerTestSuite) SetUpSuite(c *check.C) {
	go func() {
		err := ts.bs.Start()
		if err != nil {
			logger.GetLogger("broker/test").Error("start broker server error", logger.Error(err))
		}
	}()

	time.Sleep(100 * time.Millisecond)
}

func (ts *brokerTestSuite) TearDownSuite(c *check.C) {
	ts.bs.Close()
}

func (ts *brokerTestSuite) TestWritePoints(c *check.C) {
	cli := NewBrokerClient(bindAddress, timeout)

	err := cli.Init()

	c.Assert(err, check.IsNil)

	resp, err := cli.WritePoints(&common.Request{
		Data: []byte("hello"),
	})

	c.Assert(err, check.IsNil)
	c.Assert(resp.Code, check.Equals, rpc.OK)
	c.Assert(resp.Msg, check.Equals, "")

	err = cli.Close()
	c.Assert(err, check.IsNil)

}
