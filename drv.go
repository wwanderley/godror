// Copyright 2017 Tamás Gulácsi
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

// Package goracle is a database/sql/driver for Oracle DB.
//
// The connection string for the sql.Open("goracle", connString) call can be
// the simple
//   loin/password@sid [AS SYSDBA|AS SYSOPER]
//
// type (with sid being the sexp returned by tnsping),
// or in the form of
//   ora://login:password@sid/? \
//     sysdba=0& \
//     sysoper=0& \
//     poolMinSessions=1& \
//     poolMaxSessions=1000& \
//     poolIncrement=1& \
//     connectionClass=POOLED
//
// These are the defaults. Many advocate that a static session pool (min=max, incr=0)
// is better, with 1-10 sessions per CPU thread.
// See http://docs.oracle.com/cd/E82638_01/JJUCP/optimizing-real-world-performance.htm#JJUCP-GUID-BC09F045-5D80-4AF5-93F5-FEF0531E0E1D
//
// If you specify connectionClass, that'll reuse the same session pool
// without the connectionClass, but will specify it on each session acquire.
// Thus you can cluster the session pool with classes, or ose POOLED for DRCP.
package goracle

//go:generate git submodule update --init --recursive

/*
#cgo CFLAGS: -I./odpi/include -I./odpi/src

#include <stdlib.h>

#include "dpiImpl.h"
#include "dpiConn.c"
#include "dpiContext.c"
#include "dpiData.c"
#include "dpiDeqOptions.c"
#include "dpiEnqOptions.c"
#include "dpiEnv.c"
#include "dpiError.c"
#include "dpiGen.c"
#include "dpiGlobal.c"
#include "dpiLob.c"
#include "dpiMsgProps.c"
#include "dpiObject.c"
#include "dpiObjectAttr.c"
#include "dpiObjectType.c"
#include "dpiOci.c"
#include "dpiOracleType.c"
#include "dpiPool.c"
#include "dpiRowid.c"
#include "dpiStmt.c"
#include "dpiSubscr.c"
#include "dpiUtils.c"
#include "dpiVar.c"
*/
import "C"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/pkg/errors"
)

// Version of this driver
const Version = "v2.1.7"

const (
	// DefaultFetchRowCount is the number of prefetched rows by default (if not changed through ContextWithFetchRowCount).
	DefaultFetchRowCount = 1 << 8

	// DefaultArraySize is the length of the maximum PL/SQL array by default (if not changed through ContextWithArraySize).
	DefaultArraySize = 1 << 10
)

const (
	// DpiMajorVersion is the wanted major version of the underlying ODPI-C library.
	DpiMajorVersion = 2
	// DpiMinorVersion is the wanted minor version of the underlying ODPI-C library.
	DpiMinorVersion = 0

	// DriverName is set on the connection to be seen in the DB
	DriverName = "gopkg.in/goracle.v2 : " + Version

	// DefaultPoolMinSessions specifies the default value for minSessions for pool creation.
	DefaultPoolMinSessions = 1
	// DefaultPoolMaxSessions specifies the default value for maxSessions for pool creation.
	DefaultPoolMaxSessions = 1000
	// DefaultPoolInrement specifies the default value for increment for pool creation.
	DefaultPoolIncrement = 1
	// DefaultConnectionClass is the defailt connectionClass
	DefaultConnectionClass = "GORACLE"
)

// Number as string
type Number string

var (
	Int64   = intType{}
	Float64 = floatType{}
	Num     = numType{}
)

type intType struct{}

func (intType) String() string { return "Int64" }
func (intType) ConvertValue(v interface{}) (driver.Value, error) {
	Log("ConvertValue", "Int64", "value", v)
	switch x := v.(type) {
	case int32:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint64:
		return int64(x), nil
	case float32:
		if _, f := math.Modf(float64(x)); f != 0 {
			return int64(x), errors.Errorf("non-zero fractional part: %f", f)
		}
		return int64(x), nil
	case float64:
		if _, f := math.Modf(x); f != 0 {
			return int64(x), errors.Errorf("non-zero fractional part: %f", f)
		}
		return int64(x), nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseInt(x, 10, 64)
	case Number:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseInt(string(x), 10, 64)
	default:
		return nil, errors.Errorf("unknown type %T", v)
	}
}

type floatType struct{}

func (floatType) String() string { return "Float64" }
func (floatType) ConvertValue(v interface{}) (driver.Value, error) {
	Log("ConvertValue", "Float64", "value", v)
	switch x := v.(type) {
	case int32:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseFloat(x, 64)
	case Number:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseFloat(string(x), 64)
	default:
		return nil, errors.Errorf("unknown type %T", v)
	}
}

type numType struct{}

func (numType) String() string { return "Num" }
func (numType) ConvertValue(v interface{}) (driver.Value, error) {
	Log("ConvertValue", "Num", "value", v)
	switch x := v.(type) {
	case string:
		if x == "" {
			return 0, nil
		}
		return x, nil
	case Number:
		if x == "" {
			return 0, nil
		}
		return string(x), nil
	case int32, uint32, int64, uint64:
		return fmt.Sprintf("%d", x), nil
	case float32, float64:
		return fmt.Sprintf("%f", x), nil
	default:
		return nil, errors.Errorf("unknown type %T", v)
	}
}
func (n Number) String() string { return string(n) }
func (n Number) Value() (driver.Value, error) {
	return string(n), nil
}
func (n *Number) Scan(v interface{}) error {
	if v == nil {
		*n = ""
		return nil
	}
	switch x := v.(type) {
	case string:
		*n = Number(x)
	case Number:
		*n = x
	case int32, uint32, int64, uint64:
		*n = Number(fmt.Sprintf("%d", x))
	case float32, float64:
		*n = Number(fmt.Sprintf("%f", x))
	default:
		return errors.Errorf("unknown type %T", v)
	}
	return nil
}

// Log function
var Log = func(...interface{}) error { return nil }

func init() {
	d, err := newDrv()
	if err != nil {
		panic(err)
	}
	sql.Register("goracle", d)
}

var _ = driver.Driver((*drv)(nil))

type drv struct {
	dpiContext    *C.dpiContext
	clientVersion VersionInfo
	poolsMu       sync.Mutex
	pools         map[string]*C.dpiPool
}

func newDrv() (*drv, error) {
	var d drv
	err := &oraErr{}
	if C.dpiContext_create(C.uint(DpiMajorVersion), C.uint(DpiMinorVersion),
		(**C.dpiContext)(unsafe.Pointer(&d.dpiContext)), &err.errInfo,
	) == C.DPI_FAILURE {
		return nil, err
	}
	d.pools = make(map[string]*C.dpiPool)
	return &d, nil
}

// Open returns a new connection to the database.
// The name is a string in a driver-specific format.
func (d *drv) Open(connString string) (driver.Conn, error) {
	P, err := ParseConnString(connString)
	if err != nil {
		return nil, err
	}
	conn, err := d.openConn(P)
	return conn, maybeBadConn(err)
}

func (d *drv) ClientVersion() (VersionInfo, error) {
	if d.clientVersion.Version != 0 {
		return d.clientVersion, nil
	}
	var v C.dpiVersionInfo
	if C.dpiContext_getClientVersion(d.dpiContext, &v) == C.DPI_FAILURE {
		return d.clientVersion, errors.Wrap(d.getError(), "getClientVersion")
	}
	d.clientVersion.set(&v)
	return d.clientVersion, nil
}

func (d *drv) openConn(P ConnectionParams) (*conn, error) {
	c := conn{drv: d, connParams: P}
	connString := P.StringNoClass()

	defer func() { d.poolsMu.Lock(); Log("pools", d.pools, "conn", P.String()); d.poolsMu.Unlock() }()
	authMode := C.dpiAuthMode(C.DPI_MODE_AUTH_DEFAULT)
	if P.IsSysDBA {
		authMode |= C.DPI_MODE_AUTH_SYSDBA
	} else if P.IsSysOper {
		authMode |= C.DPI_MODE_AUTH_SYSOPER
	}

	extAuth := C.int(b2i(P.Username == "" && P.Password == ""))
	var connCreateParams C.dpiConnCreateParams
	if C.dpiContext_initConnCreateParams(d.dpiContext, &connCreateParams) == C.DPI_FAILURE {
		return nil, errors.Wrap(d.getError(), "initConnCreateParams")
	}
	connCreateParams.authMode = authMode
	connCreateParams.externalAuth = extAuth
	if P.ConnClass != "" {
		cConnClass := C.CString(P.ConnClass)
		defer C.free(unsafe.Pointer(cConnClass))
		connCreateParams.connectionClass = cConnClass
		connCreateParams.connectionClassLength = C.uint32_t(len(P.ConnClass))
	}
	if !(P.IsSysDBA || P.IsSysOper) {
		d.poolsMu.Lock()
		dp := d.pools[connString]
		d.poolsMu.Unlock()
		if dp != nil {
			dc := C.malloc(C.sizeof_void)
			Log("C", "dpiPool_acquireConnection", "conn", connCreateParams)
			if C.dpiPool_acquireConnection(
				dp,
				nil, 0, nil, 0, &connCreateParams,
				(**C.dpiConn)(unsafe.Pointer(&dc)),
			) == C.DPI_FAILURE {
				return nil, errors.Wrapf(d.getError(), "acquireConnection[%s]", P)
			}
			c.dpiConn = (*C.dpiConn)(dc)
			return &c, nil
		}
	}

	var cUserName, cPassword *C.char
	if !(P.Username == "" && P.Password == "") {
		cUserName, cPassword = C.CString(P.Username), C.CString(P.Password)
	}
	cSid := C.CString(P.SID)
	cUTF8, cConnClass := C.CString("AL32UTF8"), C.CString(P.ConnClass)
	cDriverName := C.CString(DriverName)
	defer func() {
		if cUserName != nil {
			C.free(unsafe.Pointer(cUserName))
			C.free(unsafe.Pointer(cPassword))
		}
		C.free(unsafe.Pointer(cSid))
		C.free(unsafe.Pointer(cUTF8))
		C.free(unsafe.Pointer(cConnClass))
		C.free(unsafe.Pointer(cDriverName))
	}()
	var commonCreateParams C.dpiCommonCreateParams
	if C.dpiContext_initCommonCreateParams(d.dpiContext, &commonCreateParams) == C.DPI_FAILURE {
		return nil, errors.Wrap(d.getError(), "initCommonCreateParams")
	}
	commonCreateParams.createMode = C.DPI_MODE_CREATE_DEFAULT | C.DPI_MODE_CREATE_THREADED | C.DPI_MODE_CREATE_EVENTS
	commonCreateParams.encoding = cUTF8
	commonCreateParams.nencoding = cUTF8
	commonCreateParams.driverName = cDriverName
	commonCreateParams.driverNameLength = C.uint32_t(len(DriverName))

	if P.IsSysDBA || P.IsSysOper {
		dc := C.malloc(C.sizeof_void)
		Log("C", "dpiConn_create", "username", P.Username, "password", P.Password, "sid", P.SID, "common", commonCreateParams, "conn", connCreateParams)
		if C.dpiConn_create(
			d.dpiContext,
			cUserName, C.uint32_t(len(P.Username)),
			cPassword, C.uint32_t(len(P.Password)),
			cSid, C.uint32_t(len(P.SID)),
			&commonCreateParams,
			&connCreateParams,
			(**C.dpiConn)(unsafe.Pointer(&dc)),
		) == C.DPI_FAILURE {
			return nil, errors.Wrapf(d.getError(), "username=%q password=%q sid=%q params=%+v", P.Username, P.Password, P.SID, connCreateParams)
		}
		c.dpiConn = (*C.dpiConn)(dc)
		return &c, nil
	}
	var poolCreateParams C.dpiPoolCreateParams
	if C.dpiContext_initPoolCreateParams(d.dpiContext, &poolCreateParams) == C.DPI_FAILURE {
		return nil, errors.Wrap(d.getError(), "initPoolCreateParams")
	}
	poolCreateParams.minSessions = C.uint32_t(P.MinSessions)
	poolCreateParams.maxSessions = C.uint32_t(P.MaxSessions)
	poolCreateParams.sessionIncrement = C.uint32_t(P.PoolIncrement)
	if extAuth == 1 {
		poolCreateParams.homogeneous = 0
	}
	poolCreateParams.externalAuth = extAuth
	poolCreateParams.getMode = C.DPI_MODE_POOL_GET_NOWAIT

	var dp *C.dpiPool
	Log("C", "dpiPool_create", "username", P.Username, "password", P.Password, "sid", P.SID, "common", commonCreateParams, "pool", poolCreateParams)
	if C.dpiPool_create(
		d.dpiContext,
		cUserName, C.uint32_t(len(P.Username)),
		cPassword, C.uint32_t(len(P.Password)),
		cSid, C.uint32_t(len(P.SID)),
		&commonCreateParams,
		&poolCreateParams,
		(**C.dpiPool)(unsafe.Pointer(&dp)),
	) == C.DPI_FAILURE {
		return nil, errors.Wrapf(d.getError(), "username=%q password=%q minSessions=%d maxSessions=%d poolIncrement=%d extAuth=%d",
			P.Username, strings.Repeat("*", len(P.Password)),
			P.MinSessions, P.MaxSessions, P.PoolIncrement, extAuth)
	}
	C.dpiPool_setTimeout(dp, 300)
	//C.dpiPool_setMaxLifetimeSession(dp, 3600)
	C.dpiPool_setStmtCacheSize(dp, 1<<20)
	d.poolsMu.Lock()
	d.pools[connString] = dp
	d.poolsMu.Unlock()

	return d.openConn(P)
}

// ConnectionParams holds the params for a connection (pool).
// You can use ConnectionParams{...}.String() as a connection string
// in sql.Open.
type ConnectionParams struct {
	Username, Password, SID, ConnClass      string
	IsSysDBA, IsSysOper                     bool
	MinSessions, MaxSessions, PoolIncrement int
}

func (P ConnectionParams) StringNoClass() string {
	return P.string(false)
}
func (P ConnectionParams) String() string {
	return P.string(true)
}

func (P ConnectionParams) string(class bool) string {
	host, path := P.SID, ""
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host, path = host[:i], host[i:]
	}
	cc := ""
	if class {
		cc = fmt.Sprintf("connectionClass=%s&", url.QueryEscape(P.ConnClass))
	}
	// params should be sorted lexicographically
	return (&url.URL{
		Scheme: "oracle",
		User:   url.UserPassword(P.Username, P.Password),
		Host:   host,
		Path:   path,
		RawQuery: cc +
			fmt.Sprintf("poolIncrement=%d&poolMaxSessions=%d&poolMinSessions=%d&"+
				"sysdba=%d&sysoper=%d",
				P.PoolIncrement, P.MaxSessions, P.MinSessions,
				b2i(P.IsSysDBA), b2i(P.IsSysOper),
			),
	}).String()
}

// ParseConnString parses the given connection string into a struct.
func ParseConnString(connString string) (ConnectionParams, error) {
	P := ConnectionParams{
		MinSessions:   DefaultPoolMinSessions,
		MaxSessions:   DefaultPoolMaxSessions,
		PoolIncrement: DefaultPoolIncrement,
		ConnClass:     DefaultConnectionClass,
	}
	if !strings.HasPrefix(connString, "oracle://") {
		i := strings.IndexByte(connString, '/')
		if i < 0 {
			return P, errors.Errorf("no / in %q", connString)
		}
		P.Username, connString = connString[:i], connString[i+1:]
		if i = strings.IndexByte(connString, '@'); i >= 0 {
			P.Password, P.SID = connString[:i], connString[i+1:]
		} else {
			P.Password = connString
			if P.SID = os.Getenv("ORACLE_SID"); P.SID == "" {
				P.SID = os.Getenv("TWO_TASK")
			}
		}
		uSid := strings.ToUpper(P.SID)
		if P.IsSysDBA = strings.HasSuffix(uSid, " AS SYSDBA"); P.IsSysDBA {
			P.SID = P.SID[:len(P.SID)-10]
		} else if P.IsSysOper = strings.HasSuffix(uSid, " AS SYSOPER"); P.IsSysOper {
			P.SID = P.SID[:len(P.SID)-11]
		}
		if strings.HasSuffix(P.SID, ":POOLED") {
			P.ConnClass, P.SID = "POOLED", P.SID[:len(P.SID)-7]
		}
		return P, nil
	}
	u, err := url.Parse(connString)
	if err != nil {
		return P, errors.Wrap(err, connString)
	}
	if usr := u.User; usr != nil {
		P.Username = usr.Username()
		P.Password, _ = usr.Password()
	}
	P.SID = u.Hostname()
	if u.Port() != "" {
		P.SID += ":" + u.Port()
	}
	if u.Path != "" && u.Path != "/" {
		P.SID += u.Path
	}
	q := u.Query()
	if vv, ok := q["connectionClass"]; ok {
		P.ConnClass = vv[0]
	}
	if P.IsSysDBA = q.Get("sysdba") == "1"; !P.IsSysDBA {
		P.IsSysOper = q.Get("sysoper") == "1"
	}

	for _, task := range []struct {
		Dest *int
		Key  string
	}{
		{&P.MinSessions, "poolMinSessions"},
		{&P.MaxSessions, "poolMaxSessions"},
		{&P.PoolIncrement, "poolIncrement"},
	} {
		s := q.Get(task.Key)
		if s == "" {
			continue
		}
		var err error
		*task.Dest, err = strconv.Atoi(s)
		if err != nil {
			return P, errors.Wrap(err, task.Key+"="+s)
		}
	}
	if P.MinSessions > P.MaxSessions {
		P.MinSessions = P.MaxSessions
	}
	if P.MinSessions == P.MaxSessions {
		P.PoolIncrement = 0
	} else if P.PoolIncrement < 1 {
		P.PoolIncrement = 1
	}

	return P, nil
}

type oraErr struct {
	errInfo C.dpiErrorInfo
}

func (oe *oraErr) Code() int       { return int(oe.errInfo.code) }
func (oe *oraErr) Message() string { return C.GoString(oe.errInfo.message) }
func (oe *oraErr) Error() string {
	msg := oe.Message()
	if oe.errInfo.code == 0 && msg == "" {
		return ""
	}
	prefix := fmt.Sprintf("ORA-%05d: ", oe.Code())
	if strings.HasPrefix(msg, prefix) {
		return msg
	}
	return prefix + msg
}

func (d *drv) getError() *oraErr {
	var oe oraErr
	C.dpiContext_getError(d.dpiContext, &oe.errInfo)
	return &oe
}

func b2i(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

type VersionInfo struct {
	Version, Release, Update, PortRelease, PortUpdate, Full int
	ServerRelease                                           string
}

func (V *VersionInfo) set(v *C.dpiVersionInfo) {
	*V = VersionInfo{
		Version: int(v.versionNum),
		Release: int(v.releaseNum), Update: int(v.updateNum),
		PortRelease: int(v.portReleaseNum), PortUpdate: int(v.portUpdateNum),
		Full: int(v.fullVersionNum),
	}
}
func (V VersionInfo) String() string {
	var s string
	if V.ServerRelease != "" {
		s = " [" + V.ServerRelease + "]"
	}
	return fmt.Sprintf("%d.%d.%d.%d.%d%s", V.Version, V.Release, V.Update, V.PortRelease, V.PortUpdate, s)
}

type ctxKey string

const logCtxKey = ctxKey("goracle.Log")

type logFunc func(...interface{}) error

func ctxGetLog(ctx context.Context) logFunc {
	if lgr, ok := ctx.Value(logCtxKey).(func(...interface{}) error); ok {
		return lgr
	}
	return Log
}

func ContextWithLog(ctx context.Context, logF func(...interface{}) error) context.Context {
	return context.WithValue(ctx, logCtxKey, logF)
}
