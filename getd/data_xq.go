package getd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/carusyte/stock/conf"
	"github.com/carusyte/stock/global"
	"github.com/carusyte/stock/model"
	"github.com/carusyte/stock/util"
	"github.com/pkg/errors"
	"github.com/ssgreg/repeat"
)

func getKlineXQ(stk *model.Stock, kltype []model.DBTab) (
	tdmap map[model.DBTab]*model.TradeData, lkmap map[model.DBTab]int, suc bool) {

	tdmap = make(map[model.DBTab]*model.TradeData)
	lkmap = make(map[model.DBTab]int)
	code := stk.Code
	xdxr := latestUFRXdxr(stk.Code)

	genop := func(tab model.DBTab) (op func(c int) (e error)) {
		return func(c int) (e error) {
			trdat, lklid, suc, retry := xqKline(stk, tab, xdxr)
			if suc {
				log.Infof("%s %v fetched: %d", code, tab, trdat.MaxLen())
				tdmap[tab] = trdat
				lkmap[tab] = lklid
				return nil
			}
			e = fmt.Errorf("failed to get kline for %s", code)
			if retry {
				log.Printf("%s retrying [%d]", code, c+1)
				return repeat.HintTemporary(e)
			}
			return repeat.HintStop(e)
		}
	}

	suc = true
	for _, klt := range kltype {
		e := repeat.Repeat(
			repeat.FnWithCounter(genop(klt)),
			repeat.StopOnSuccess(),
			repeat.LimitMaxTries(conf.Args.DataSource.KlineFailureRetry-1),
			repeat.WithDelay(
				repeat.FullJitterBackoff(500*time.Millisecond).WithMaxDelay(5*time.Second).Set(),
			),
		)
		if e != nil {
			suc = false
		}
	}

	return tdmap, lkmap, suc
}

func xqKline(stk *model.Stock, tab model.DBTab, xdxr *model.Xdxr) (
	trdat *model.TradeData, lklid int, suc, retry bool) {

	period := ""
	xdrType := "normal"
	rtype := model.None
	var cycle model.CYTP
	switch tab {
	case model.KLINE_DAY_F, model.KLINE_DAY_NR, model.KLINE_DAY_B:
		period = "day"
		cycle = model.DAY
	case model.KLINE_WEEK_F, model.KLINE_WEEK_NR, model.KLINE_WEEK_B:
		period = "week"
		cycle = model.WEEK
	case model.KLINE_MONTH_F, model.KLINE_MONTH_NR, model.KLINE_MONTH_B:
		period = "month"
		cycle = model.MONTH
	default:
		log.Panicf("unsupported type: %+v", tab)
	}
	switch tab {
	case model.KLINE_DAY_F, model.KLINE_WEEK_F, model.KLINE_MONTH_F:
		xdrType = "after"
		rtype = model.Forward
	case model.KLINE_DAY_B, model.KLINE_WEEK_B, model.KLINE_MONTH_B:
		xdrType = "before"
		rtype = model.Backward
	}
	mkt := strings.ToUpper(stk.Market.String)
	symbol := mkt + stk.Code
	code := stk.Code
	if isIndex(symbol) {
		code = symbol
	}
	incr := true
	if rtype == model.Forward {
		incr = xdxr == nil
	}
	lklid = -1
	ldate := ""
	if incr {
		ldy := getLatestTradeDataBasic(code, cycle, rtype, 5+1) //plus one offset for pre-close, varate calculation
		if ldy != nil {
			ldate = ldy.Date
			lklid = ldy.Klid
		} else {
			log.Printf("%s latest %s data not found, will be fully refreshed", code, tab)
		}
	} else {
		log.Printf("%s %s data will be fully refreshed", code, tab)
	}

	startDateStr := "1990-12-19"
	startDate, e := time.Parse(global.DateFormat, startDateStr)
	if e != nil {
		log.Panicf("failed to parse date %s: %+v", startDateStr, e)
	}
	count := -142
	multiGet := false
	if lklid == -1 {
		count = int(math.Round(-.75*(time.Since(startDate).Hours()/24.) - float64(rand.Intn(1000))))
		multiGet = true
	} else {
		ltime, e := time.Parse(global.DateFormat, ldate)
		if e != nil {
			log.Warnf("%s %+v failed to parse date value '%s': %+v", stk.Code, tab, ldate, e)
			return nil, lklid, false, false
		}
		count = -1 * (int(time.Since(ltime).Hours()/24) + 2)
	}

	xqk, e := tryXQKline(code, symbol, period, xdrType, count, multiGet)
	if e != nil {
		return nil, lklid, false, true
	}

	if e = fixXqAmount(xqk); e != nil {
		return nil, lklid, false, true
	}

	//construct trade data
	trdat = &model.TradeData{
		Code:          stk.Code,
		Cycle:         cycle,
		Reinstatement: rtype,
		Base:          xqk.GetData(false),
	}

	return trdat, lklid, true, false
}

//supplement missing "amount" from validate table if any
func fixXqAmount(k *model.XQKline) (e error) {
	if len(k.MissingAmount) == 0 {
		return
	}
	trdat := GetTrDataAt(k.Code, TrDataQry{Validate: true, Basic: true},
		Date, false, util.Str2IntfSlice(k.MissingAmount)...)
	for _, b := range trdat.Base {
		k.Data[b.Date].Amount = b.Amount
	}
	return
}

func tryXQCookie() (cookies []*http.Cookie, px *util.Proxy, headers map[string]string, e error) {
	op := func() error {
		cookies, px, headers, e = xqCookie()
		if e != nil {
			return repeat.HintTemporary(e)
		}
		return nil
	}
	e = repeat.Repeat(
		repeat.Fn(op),
		repeat.StopOnSuccess(),
		repeat.LimitMaxTries(conf.Args.DefaultRetry),
	)
	if e != nil {
		log.Warnf("failed to get cookies from XQ: %+v", e)
	}
	return
}

func tryXQKline(code, symbol, period, xdrType string, count int, multiGet bool) (xqk *model.XQKline, e error) {
	xqk = &model.XQKline{Code: code}
	//symbol = SH600104
	//begin = 1579589390096
	//period = day/week/month/60m/120m...
	//type = normal/after(forward)/before(backward)
	//count = -1000
	urlt := `https://stock.xueqiu.com/v5/stock/chart/kline.json?` +
		`symbol=%[1]s&begin=%[2]d&period=%[3]s&type=%[4]s&count=%[5]d&indicator=kline`
	begin := util.UnixMilliseconds(time.Now().AddDate(0, 0, 1))
	url := fmt.Sprintf(urlt, symbol, begin, period, xdrType, count)
	RETRY := 5
	genop := func(url string, hd map[string]string,
		px *util.Proxy, ck []*http.Cookie) (op func(c int) error) {
		return func(c int) error {
			res, e := util.HTTPGet(url, hd, px, ck...)
			if e != nil {
				log.Warnf("%s HTTP failed from %s: %+v", code, url, e)
				return repeat.HintTemporary(e)
			}
			defer res.Body.Close()
			data, e := ioutil.ReadAll(res.Body)
			if e != nil {
				log.Warnf("%s failed to read http response body from %s: %+v", code, url, e)
				return repeat.HintTemporary(e)
			}
			e = json.Unmarshal(data, xqk)
			if e != nil {
				if strings.Contains(e.Error(), "400016") { //cookie timeout
					log.Warnf("%s cookie timeout for %s: %+v", code, url, e)
					return repeat.HintStop(e)
				}
				log.Warnf("%s failed to parse json from %s: %+v\return value:%+v", code, url, e, string(data))
				return repeat.HintTemporary(e)
			}
			log.Debugf("return from XQ: %+v", string(data))
			return nil
		}
	}
	var (
		ck []*http.Cookie
		px *util.Proxy
		hd map[string]string
	)
	//first get the cookies from home page
	if ck, px, hd, e = tryXQCookie(); e != nil {
		return
	}
	//get kline using same header, proxy and cookies
	if e = repeat.Repeat(
		repeat.FnWithCounter(genop(url, hd, px, ck)),
		repeat.StopOnSuccess(),
		repeat.LimitMaxTries(RETRY),
		repeat.WithDelay(repeat.FullJitterBackoff(200*time.Millisecond).WithMaxDelay(2*time.Second).Set()),
	); e != nil {
		return
	}
	//check if more data is required
	ckTimeout := 0
	for multiGet && xqk.NumAdded == count && ckTimeout < 2 {
		data := xqk.GetData(false)
		var startDate time.Time
		startDate, e = time.Parse(global.DateFormat, data[0].Date)
		if e != nil {
			log.Warnf("%s failed to parse date %s: %+v", code, data[0].Date, e)
			return
		}
		begin = util.UnixMilliseconds(startDate.AddDate(0, 0, -1))
		url = fmt.Sprintf(urlt, symbol, begin, period, xdrType, count)
		if e = repeat.Repeat(
			repeat.FnWithCounter(genop(url, hd, px, ck)),
			repeat.StopOnSuccess(),
			repeat.LimitMaxTries(RETRY),
			repeat.WithDelay(repeat.FullJitterBackoff(200*time.Millisecond).WithMaxDelay(2*time.Second).Set()),
		); e != nil {
			if strings.Contains(e.Error(), "400016") { //cookie timeout, refresh cookie then retry
				if ck, px, hd, e = tryXQCookie(); e != nil {
					return
				}
				ckTimeout++
				continue
			}
			return
		}
		ckTimeout = 0
	}
	return
}

func xqCookie() (cookies []*http.Cookie, px *util.Proxy, headers map[string]string, e error) {
	homePage := `https://xueqiu.com/`
	var uagent string
	uagent, e = util.PickUserAgent()
	if e != nil {
		e = errors.Wrap(e, "failed to get user agent")
		return
	}
	headers = map[string]string{
		"User-Agent": uagent,
	}
	wgt := conf.Args.DataSource.XQ.DirectProxyWeight
	sum := wgt[0] + wgt[1] + wgt[2]
	dw := wgt[0] / sum
	mw := (wgt[0] + wgt[1]) / sum
	dice := rand.Float64()
	if dice <= dw {
		//direct connection
		log.Debug("accessing XQ using direct connection")
	} else if dice <= mw {
		//master proxy
		log.Debugf("accessing XQ using master proxy: %s", conf.Args.Network.MasterProxyAddr)
		ss := strings.Split(conf.Args.Network.MasterProxyAddr, ":")
		px = &util.Proxy{
			Host: ss[0],
			Port: ss[1],
			Type: "socks5",
		}
	} else {
		//rotate proxy
		px, e = util.PickProxy()
		if e != nil {
			e = errors.Wrap(e, "failed to get rotate proxy")
			return
		}
		log.Debugf("accessing XQ using rotate proxy: %s://%s:%s", px.Type, px.Host, px.Port)
	}
	res, e := util.HTTPGet(homePage, headers, px)
	if e != nil {
		e = errors.Wrap(e, "failed to get http response")
		return
	}
	defer res.Body.Close()
	cookies = res.Cookies()
	return
}

func xqShares(stock *model.Stock, px *util.Proxy, headers map[string]string, cookies []*http.Cookie) (ok, retry bool) {
	// get share info from xueqiu.com
	// https://xueqiu.com/snowman/S/SH601598/detail#/GBJG
	// https://stock.xueqiu.com/v5/stock/f10/cn/shareschg.json?symbol=SH601598&count=100&extend=true
	url := fmt.Sprintf(`https://stock.xueqiu.com/v5/stock/f10/cn/shareschg.json?symbol=%s%s&count=1000&extend=true`, stock.Market.String, stock.Code)
	res, e := util.HTTPGet(url, headers, px, cookies...)
	if e != nil {
		log.Printf("%s, http failed %s", stock.Code, url)
		return false, true
	}
	defer res.Body.Close()
	var xqshare model.XqSharesChg
	if body, e := ioutil.ReadAll(res.Body); e != nil {
		log.Printf("[%s,%s] failed to read from response body, retrying...", stock.Code,
			stock.Name)
		return false, true
	} else if strings.Contains(string(body), `"error_code": "400016"`) {
		log.Warnf("%s cookie timeout: %+v", stock.Code, string(body))
		return false, true
	} else if e = json.Unmarshal(body, &xqshare); e != nil {
		log.Printf("[%s,%s] failed to parse json body, retrying...", stock.Code,
			stock.Name)
		return false, true
	}
	if xqshare.ErrorCode != 0 {
		log.Printf("[%s,%s] failed from xueqiu.com:[%d, %s] retrying...", stock.Code,
			stock.Name, xqshare.ErrorCode, xqshare.ErrorDesc)
		return false, true
	} else if len(xqshare.Data.Items) == 0 {
		log.Printf("[%s,%s] no share info from xueqiu.com", stock.Code, stock.Name)
		return true, false
	}
	mod := 0.00000001
	s := xqshare.Data.Items[0]
	if s.TotalShare != nil {
		stock.ShareSum.Valid = true
		stock.ShareSum.Float64 = *s.TotalShare * mod
	}
	if s.FloatAShare != nil {
		stock.AShareSum.Valid = true
		stock.AShareSum.Float64 += *s.FloatAShare
		stock.AShareExch.Valid = true
		stock.AShareExch.Float64 = *s.FloatAShare * mod
	}
	if s.LimitAShare != nil {
		stock.AShareSum.Valid = true
		stock.AShareSum.Float64 += *s.LimitAShare
		stock.AShareR.Valid = true
		stock.AShareR.Float64 = *s.LimitAShare * mod
	}
	stock.AShareSum.Float64 *= mod

	if s.FloatBShare != nil {
		stock.BShareSum.Valid = true
		stock.BShareSum.Float64 += *s.FloatBShare
		stock.BShareExch.Valid = true
		stock.BShareExch.Float64 = *s.FloatBShare * mod
	}
	if s.LimitBShare != nil {
		stock.BShareSum.Valid = true
		stock.BShareSum.Float64 += *s.LimitBShare
		stock.BShareR.Valid = true
		stock.BShareR.Float64 = *s.LimitBShare * mod
	}
	stock.BShareSum.Float64 *= mod

	if s.FloatHShare != nil {
		stock.HShareSum.Valid = true
		stock.HShareSum.Float64 += *s.FloatHShare
		stock.HShareExch.Valid = true
		stock.HShareExch.Float64 = *s.FloatHShare * mod
	}
	if s.LimitHShare != nil {
		stock.HShareSum.Valid = true
		stock.HShareSum.Float64 += *s.LimitHShare
		stock.HShareR.Valid = true
		stock.HShareR.Float64 = *s.LimitHShare * mod
	}
	stock.HShareSum.Float64 *= mod

	return true, false
}