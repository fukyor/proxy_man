package mproxy

import (
	"net"
)

// tunnelTrafficReader 统计读取的流量
type tunnelTrafficClient struct {
	halfClosable
	nread int64
	nwrite int64
	onUpdate func()
}

func newTunnelTrafficClient(conn net.Conn) (*tunnelTrafficClient, bool) {
	if clientReader, ok := conn.(halfClosable); ok {
		return &tunnelTrafficClient{
			halfClosable: clientReader,	
			nread: 0,
			nwrite: 0,
		}, true
	}else{
		return nil, false
	}
}

func (r *tunnelTrafficClient) Read(p []byte) (n int, err error) {
	n, err = r.halfClosable.Read(p)
	r.nread += int64(n)
	GlobalTrafficUp.Add(int64(n)) // 客户端连接读取，Read = 上行流量
	return n, err
}

func (w *tunnelTrafficClient) Write(p []byte) (n int, err error) {
	n, err = w.halfClosable.Write(p)
	w.nwrite += int64(n)
	GlobalTrafficDown.Add(int64(n)) // 客户端连接响应，write = 下行流量
	return n, err
}

func (c *tunnelTrafficClient) Close() error {
	c.onUpdate()
	c.onUpdate = nil
	return c.halfClosable.Close()
}



type tunnelTrafficClientNoClosable struct {
	conn net.Conn
	nread int64
	nwrite int64
	onUpdate func()
}

func newtunnelTrafficClientNoClosable(conn net.Conn) (*tunnelTrafficClientNoClosable){
	return &tunnelTrafficClientNoClosable{
		conn: conn,
		nread: 0,
		nwrite: 0,
	}
}

func (r *tunnelTrafficClientNoClosable) Read(p []byte) (n int, err error) {
	n, err = r.conn.Read(p)
	r.nread += int64(n)
	GlobalTrafficUp.Add(int64(n)) // 客户端连接读取，Read = 上行流量
	return n, err
}

func (w *tunnelTrafficClientNoClosable) Write(p []byte) (n int, err error) {
	n, err = w.conn.Write(p)
	w.nwrite += int64(n)
	GlobalTrafficDown.Add(int64(n)) // 客户端连接响应，write = 下行流量
	return n, err
}

func (c *tunnelTrafficClientNoClosable) Close() error {
	c.onUpdate()
	c.onUpdate = nil
	return c.conn.Close()
}



// type tunnelTrafficTargetNoClosable struct {
// 	conn net.Conn
// 	nwrite int64
// }

// func newtunnelTrafficTargetNoClosable(conn net.Conn) (*tunnelTrafficTargetNoClosable){
// 	return &tunnelTrafficTargetNoClosable{
// 		conn: conn,
// 		nwrite: 0,
// 	}
// }

// func (r *tunnelTrafficTargetNoClosable) Read(p []byte) (n int, err error) {
// 	n, err = r.conn.Read(p)
// 	return n, err
// }

// func (w *tunnelTrafficTargetNoClosable) Write(p []byte) (n int, err error) {
// 	n, err = w.conn.Write(p)
// 	w.nwrite += int64(n)
// 	return n, err
// }

// func (c *tunnelTrafficTargetNoClosable) Close() error {
// 	return c.conn.Close()
// }





