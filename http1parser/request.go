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

	// 先把socket数据读到缓冲区并解析。Buffered记录剩余缓冲区未解析字节数
	// 并用标准库头部解析为request结构体。
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

func getRequestReader(r *bufio.Reader, cloned *bytes.Buffer) *textproto.Reader {


	data := cloned.Next(cloned.Len() - r.Buffered())
	return &textproto.Reader{
		R: bufio.NewReader(bytes.NewReader(data)),
	}
}
