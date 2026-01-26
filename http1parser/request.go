package http1parser

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/textproto"
)

type RequestReader struct {
	preventCanonicalization bool
	reader                  *bufio.Reader
	// Used only when preventCanonicalization value is true
	cloned *bytes.Buffer
}

func NewRequestReader(preventCanonicalization bool, conn io.Reader) *RequestReader {
	if !preventCanonicalization {
		return &RequestReader{
			preventCanonicalization: false,
			reader:                  bufio.NewReader(conn),
		}
	}

	var cloned bytes.Buffer
	reader := bufio.NewReader(io.TeeReader(conn, &cloned))
	return &RequestReader{
		preventCanonicalization: true,
		reader:                  reader,
		cloned:                  &cloned,
	}
}

// IsEOF returns true if there is no more data that can be read from the
// buffer and the underlying connection is closed.
func (r *RequestReader) IsEOF() bool {
	_, err := r.reader.Peek(1)
	return errors.Is(err, io.EOF)
}

// Reader is used to take over the buffered connection data
// (e.g. with HTTP/2 data).
// After calling this function, make sure to consume all the data related
// to the current request.
func (r *RequestReader) Reader() *bufio.Reader {
	return r.reader
}

func (r *RequestReader) ReadRequest() (*http.Request, error) {
	if !r.preventCanonicalization {
		// 使用标准库对http头部进行解析
		return http.ReadRequest(r.reader)
	}

	// 用标准库完整解析原始套接字中收到的二进制流中的头部为request结构体。（注意是只解析了头部）
	// 如果自己解析就是proxylab里面的按照/r/n按行解析，容易在复杂网络环境下出错，所以用标准库
	req, err := http.ReadRequest(r.reader)
	if err != nil {
		return nil, err
	}

	// 准确提取出请求头部
	httpDataReader := getRequestReader(r.reader, r.cloned)
	// 将头部:前的字段全部提取出来
	headers, _ := Http1ExtractHeaders(httpDataReader)

	// 将标准头部字段全部替换为用户的非标头部
	for _, headerName := range headers {
		canonicalizedName := textproto.CanonicalMIMEHeaderKey(headerName)
		if canonicalizedName == headerName {
			continue
		}

		// Rewrite header keys to the non-canonical parsed value
		values, ok := req.Header[canonicalizedName]
		if ok {
			req.Header.Del(canonicalizedName)
			req.Header[headerName] = values
		}
	}

	return req, nil
}


/*
[-------------------- cloned维护的buffer (bytes.Buffer) (500字节) -------------]
[  待读取数据 .................................................................]
^
0 (cloned 的当前指针,)

|_____________________________________________________________________________|
                   这个区间长度就是 cloned.Len() = 500


[-------------------- bufio.Reader 维护的 Buffer (500字节) --------------------]
[  已读取数据 (100)  ] [              未读取数据 (400)                     		]
^                    ^                                                   	 ^
0 (起始)             R (当前读取指针)                                        W (结束)

                     |________________________________________________________|
                                这个区间长度就是 r.Buffered() = 400
*/
// r.buf和clone.buf数据是一样的。但是里面既包含了reqheader也包含了reqbody
// 所以我们要准确的把头部获取到，而不应该误读body
// 我们最后就是得到已读的100字节，这是clone中保存的原始请求头副本
func getRequestReader(r *bufio.Reader, cloned *bytes.Buffer) *textproto.Reader {
	data := cloned.Next(cloned.Len() - r.Buffered())
	return &textproto.Reader{
		R: bufio.NewReader(bytes.NewReader(data)),
	}
}
