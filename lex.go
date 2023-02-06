// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Reading of PDF tokens and objects from a raw byte stream.

package pdf

import (
	"fmt"
	"io"
	"strconv"
)

// A token is a PDF token in the input stream, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	keyword, a PDF keyword
//	name, a PDF name without the leading slash
//
type token interface{}

// A name is a PDF name, without the leading slash.
type name string

// A keyword is a PDF keyword.
// Delimiter tokens used in higher-level syntax,
// such as "<<", ">>", "[", "]", "{", "}", are also treated as keywords.
type keyword string

// A buffer holds buffered input bytes from the PDF file.
type buffer struct {
	r           io.Reader // source of data
	buf         []byte    // buffered data
	pos         int       // read index in buf
	offset      int64     // offset at end of buf; aka offset of next read
	tmp         []byte    // scratch space for accumulating token
	unread      []token   // queue of read but then unread tokens
	allowEOF    bool
	allowObjptr bool
	allowStream bool
	eof         bool
	key         []byte
	useAES      bool
	objptr      objptr
}

// newBuffer returns a new buffer reading from r at the given offset.
func newBuffer(r io.Reader, offset int64) *buffer {
	return &buffer{
		r:           r,
		offset:      offset,
		buf:         make([]byte, 0, 4096),
		allowObjptr: true,
		allowStream: true,
	}
}

func (b *buffer) seek(offset int64) {
	b.offset = offset
	b.buf = b.buf[:0]
	b.pos = 0
	b.unread = b.unread[:0]
}

func (b *buffer) readByte() byte {
	if b.pos >= len(b.buf) {
		b.reload()
		if b.pos >= len(b.buf) {
			return '\n'
		}
	}
	c := b.buf[b.pos]
	b.pos++
	return c
}

func (b *buffer) reload() (bool, error) {
	n := cap(b.buf) - int(b.offset%int64(cap(b.buf)))
	n, err := b.r.Read(b.buf[:n])
	if n == 0 && err != nil {
		b.buf = b.buf[:0]
		b.pos = 0
		if b.allowEOF && err == io.EOF {
			b.eof = true
			return false, nil
		}
		return false, fmt.Errorf("malformed PDF: reading at offset %d: %v", b.offset, err)
	}
	b.offset += int64(n)
	b.buf = b.buf[:n]
	b.pos = 0
	return true, nil
}

func (b *buffer) seekForward(offset int64) error {
	for b.offset < offset {
		reload, err := b.reload()
		if err != nil {
			return err
		}
		if !reload {
			return nil
		}
	}
	b.pos = len(b.buf) - int(b.offset-offset)
	return nil
}

func (b *buffer) readOffset() int64 {
	return b.offset - int64(len(b.buf)) + int64(b.pos)
}

func (b *buffer) unreadByte() {
	if b.pos > 0 {
		b.pos--
	}
}

func (b *buffer) unreadToken(t token) {
	b.unread = append(b.unread, t)
}

func (b *buffer) readToken() (token, error) {
	if n := len(b.unread); n > 0 {
		t := b.unread[n-1]
		b.unread = b.unread[:n-1]
		return t, nil
	}

	// Find first non-space, non-comment byte.
	c := b.readByte()
	for {
		if isSpace(c) {
			if b.eof {
				return io.EOF, nil
			}
			c = b.readByte()
		} else if c == '%' {
			for c != '\r' && c != '\n' {
				c = b.readByte()
			}
		} else {
			break
		}
	}

	switch c {
	case '<':
		if b.readByte() == '<' {
			return keyword("<<"), nil
		}
		b.unreadByte()
		return b.readHexString()

	case '(':
		return b.readLiteralString()

	case '[', ']', '{', '}':
		return keyword(string(c)), nil

	case '/':
		return b.readName()

	case '>':
		if b.readByte() == '>' {
			return keyword(">>"), nil
		}
		b.unreadByte()
		fallthrough

	default:
		if isDelim(c) {
			return nil, fmt.Errorf("unexpected delimiter %#q", rune(c))
		}
		b.unreadByte()
		return b.readKeyword()
	}
}

func (b *buffer) readHexString() (token, error) {
	tmp := b.tmp[:0]
	for {
	Loop:
		c := b.readByte()
		if c == '>' {
			break
		}
		if isSpace(c) {
			goto Loop
		}
	Loop2:
		c2 := b.readByte()
		if isSpace(c2) {
			goto Loop2
		}
		x := unhex(c)<<4 | unhex(c2)
		if x < 0 {
			return "", fmt.Errorf("malformed hex string %c %c %s", c, c2, b.buf[b.pos:])
		}
		tmp = append(tmp, byte(x))
	}
	b.tmp = tmp
	return string(tmp), nil
}

func unhex(b byte) int {
	switch {
	case '0' <= b && b <= '9':
		return int(b) - '0'
	case 'a' <= b && b <= 'f':
		return int(b) - 'a' + 10
	case 'A' <= b && b <= 'F':
		return int(b) - 'A' + 10
	}
	return -1
}

func (b *buffer) readLiteralString() (token, error) {
	tmp := b.tmp[:0]
	depth := 1
Loop:
	for !b.eof {
		c := b.readByte()
		switch c {
		default:
			tmp = append(tmp, c)
		case '(':
			depth++
			tmp = append(tmp, c)
		case ')':
			if depth--; depth == 0 {
				break Loop
			}
			tmp = append(tmp, c)
		case '\\':
			switch c = b.readByte(); c {
			default:
				return tmp, fmt.Errorf("invalid escape sequence \\%c", c)
				//tmp = append(tmp, '\\', c)
			case 'n':
				tmp = append(tmp, '\n')
			case 'r':
				tmp = append(tmp, '\r')
			case 'b':
				tmp = append(tmp, '\b')
			case 't':
				tmp = append(tmp, '\t')
			case 'f':
				tmp = append(tmp, '\f')
			case '(', ')', '\\':
				tmp = append(tmp, c)
			case '\r':
				if b.readByte() != '\n' {
					b.unreadByte()
				}
				fallthrough
			case '\n':
				// no append
			case '0', '1', '2', '3', '4', '5', '6', '7':
				x := int(c - '0')
				for i := 0; i < 2; i++ {
					c = b.readByte()
					if c < '0' || c > '7' {
						b.unreadByte()
						break
					}
					x = x*8 + int(c-'0')
				}
				if x > 255 {
					return tmp, fmt.Errorf("invalid octal escape \\%03o", x)
				}
				tmp = append(tmp, byte(x))
			}
		}
	}
	b.tmp = tmp
	return string(tmp), nil
}

func (b *buffer) readName() (token, error) {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		if c == '#' {
			x := unhex(b.readByte())<<4 | unhex(b.readByte())
			if x < 0 {
				return "", fmt.Errorf("malformed name")
			}
			tmp = append(tmp, byte(x))
			continue
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	return name(string(tmp)), nil
}

func (b *buffer) readKeyword() (token, error) {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	s := string(tmp)
	switch {
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case isInteger(s):
		x, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return x, fmt.Errorf("invalid integer %s", s)
		}
		return x, nil
	case isReal(s):
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return x, fmt.Errorf("invalid real %s", s)
		}
		return x, nil
	}
	return keyword(string(tmp)), nil
}

func isInteger(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || '9' < c {
			return false
		}
	}
	return true
}

func isReal(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	ndot := 0
	for _, c := range s {
		if c == '.' {
			ndot++
			continue
		}
		if c < '0' || '9' < c {
			return false
		}
	}
	return ndot == 1
}

// An object is a PDF syntax object, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	name, a PDF name without the leading slash
//	dict, a PDF dictionary
//	array, a PDF array
//	stream, a PDF stream
//	objptr, a PDF object reference
//	objdef, a PDF object definition
//
// An object may also be nil, to represent the PDF null.
type object interface{}

type dict map[name]object

type array []object

type stream struct {
	hdr    dict
	ptr    objptr
	offset int64
}

type objptr struct {
	id  uint32
	gen uint16
}

type objdef struct {
	ptr objptr
	obj object
}

func (b *buffer) readObject() (object, error) {
	tok, err := b.readToken()
	if err != nil {
		return nil, err
	}
	if kw, ok := tok.(keyword); ok {
		switch kw {
		case "null":
			return nil, nil
		case "<<":
			return b.readDict()
		case "[":
			return b.readArray()
		}
		return nil, fmt.Errorf("unexpected keyword %q parsing object", kw)
	}

	if str, ok := tok.(string); ok && b.key != nil && b.objptr.id != 0 {
		tok, err = decryptString(b.key, b.useAES, b.objptr, str)
		if err != nil {
			return nil, err
		}
	}

	if !b.allowObjptr {
		return tok, nil
	}

	if t1, ok := tok.(int64); ok && int64(uint32(t1)) == t1 {
		tok2, err := b.readToken()
		if err != nil {
			return nil, err
		}
		if t2, ok := tok2.(int64); ok && int64(uint16(t2)) == t2 {
			tok3, err := b.readToken()
			if err != nil {
				return nil, err
			}
			switch tok3 {
			case keyword("R"):
				return objptr{uint32(t1), uint16(t2)}, nil
			case keyword("obj"):
				old := b.objptr
				b.objptr = objptr{uint32(t1), uint16(t2)}
				obj, err := b.readObject()
				if err != nil {
					return nil, err
				}
				if _, ok := obj.(stream); !ok {
					tok4, err := b.readToken()
					if err != nil {
						return nil, err
					}
					if tok4 != keyword("endobj") {
						b.unreadToken(tok4)
						return nil, fmt.Errorf("missing endobj after indirect object definition")
					}
				}
				b.objptr = old
				return objdef{objptr{uint32(t1), uint16(t2)}, obj}, nil
			}
			b.unreadToken(tok3)
		}
		b.unreadToken(tok2)
	}
	return tok, nil
}

func (b *buffer) readArray() (object, error) {
	var x array
	for {
		tok, err := b.readToken()
		if err != nil {
			return nil, err
		}
		if tok == nil || tok == keyword("]") {
			break
		}
		b.unreadToken(tok)
		obj, err := b.readObject()
		if err != nil {
			return nil, err
		}
		x = append(x, obj)
	}
	return x, nil
}

func (b *buffer) readDict() (object, error) {
	x := make(dict)
	for {
		tok, err := b.readToken()
		if err != nil {
			return nil, err
		}
		if tok == nil || tok == keyword(">>") {
			break
		}
		n, ok := tok.(name)
		if !ok {
			return nil, fmt.Errorf("unexpected non-name key %T(%v) parsing dictionary", tok, tok)
		}
		obj, err := b.readObject()
		if err != nil {
			return nil, err
		}
		x[n] = obj
	}

	if !b.allowStream {
		return x, nil
	}

	tok, err := b.readToken()
	if err != nil {
		return nil, err
	}
	if tok != keyword("stream") {
		b.unreadToken(tok)
		return x, nil
	}

	switch b.readByte() {
	case '\r':
		if b.readByte() != '\n' {
			b.unreadByte()
		}
	case '\n':
		// ok
	default:
		return nil, fmt.Errorf("stream keyword not followed by newline")
	}

	return stream{x, b.objptr, b.readOffset()}, nil
}

func isSpace(b byte) bool {
	switch b {
	case '\x00', '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelim(b byte) bool {
	switch b {
	case '<', '>', '(', ')', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}
