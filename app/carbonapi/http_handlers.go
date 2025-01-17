package carbonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bookingcom/carbonapi/pkg/handlerlog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bookingcom/carbonapi/carbonapipb"
	"github.com/bookingcom/carbonapi/date"
	"github.com/bookingcom/carbonapi/expr"
	"github.com/bookingcom/carbonapi/expr/functions/cairo/png"
	"github.com/bookingcom/carbonapi/expr/interfaces"
	"github.com/bookingcom/carbonapi/expr/metadata"
	"github.com/bookingcom/carbonapi/expr/types"
	"github.com/bookingcom/carbonapi/pkg/parser"
	dataTypes "github.com/bookingcom/carbonapi/pkg/types"
	"github.com/bookingcom/carbonapi/pkg/types/encoding/carbonapi_v2"
	ourJson "github.com/bookingcom/carbonapi/pkg/types/encoding/json"
	"github.com/bookingcom/carbonapi/pkg/types/encoding/pickle"
	"github.com/bookingcom/carbonapi/util"

	"errors"

	"go.opentelemetry.io/otel/api/kv"
	"go.opentelemetry.io/otel/api/trace"
	"go.uber.org/zap"
)

const (
	jsonFormat      = "json"
	treejsonFormat  = "treejson"
	pngFormat       = "png"
	csvFormat       = "csv"
	rawFormat       = "raw"
	svgFormat       = "svg"
	protobufFormat  = "protobuf"
	protobuf3Format = "protobuf3"
	pickleFormat    = "pickle"
	completerFormat = "completer"
)

// for testing
// TODO (grzkv): Clean up
var timeNow = time.Now

func (app *App) validateRequest(h handlerlog.HandlerWithLogger, handler string, logger *zap.Logger) http.HandlerFunc {
	t0 := time.Now()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.requestBlocker.ShouldBlockRequest(r) {
			toLog := carbonapipb.NewAccessLogDetails(r, handler, &app.config)
			toLog.HttpCode = http.StatusForbidden
			defer func() {
				app.deferredAccessLogging(logger, r, &toLog, t0, true)
			}()
			w.WriteHeader(http.StatusForbidden)
		} else {
			h(w, r, logger)
		}
	})
}

func writeResponse(ctx context.Context, w http.ResponseWriter, b []byte, format string, jsonp string) error {
	var err error
	w.Header().Set("X-Carbonapi-UUID", util.GetUUID(ctx))
	switch format {
	case jsonFormat:
		if jsonp != "" {
			w.Header().Set("Content-Type", contentTypeJavaScript)
			if _, err = w.Write([]byte(jsonp)); err != nil {
				return err
			}
			if _, err = w.Write([]byte{'('}); err != nil {
				return err
			}
			if _, err = w.Write(b); err != nil {
				return err
			}
			if _, err = w.Write([]byte{')'}); err != nil {
				return err
			}
		} else {
			w.Header().Set("Content-Type", contentTypeJSON)
			if _, err = w.Write(b); err != nil {
				return err
			}
		}
	case protobufFormat, protobuf3Format:
		w.Header().Set("Content-Type", contentTypeProtobuf)
		if _, err = w.Write(b); err != nil {
			return err
		}
	case rawFormat:
		w.Header().Set("Content-Type", contentTypeRaw)
		if _, err = w.Write(b); err != nil {
			return err
		}
	case pickleFormat:
		w.Header().Set("Content-Type", contentTypePickle)
		if _, err = w.Write(b); err != nil {
			return err
		}
	case csvFormat:
		w.Header().Set("Content-Type", contentTypeCSV)
		if _, err = w.Write(b); err != nil {
			return err
		}
	case pngFormat:
		w.Header().Set("Content-Type", contentTypePNG)
		if _, err = w.Write(b); err != nil {
			return err
		}
	case svgFormat:
		w.Header().Set("Content-Type", contentTypeSVG)
		if _, err = w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

const (
	contentTypeJSON       = "application/json"
	contentTypeProtobuf   = "application/x-protobuf"
	contentTypeJavaScript = "text/javascript"
	contentTypeRaw        = "text/plain"
	contentTypePickle     = "application/pickle"
	contentTypePNG        = "image/png"
	contentTypeCSV        = "text/csv"
	contentTypeSVG        = "image/svg+xml"
)

type renderResponse struct {
	data  []*types.MetricData
	error error
}

func (app *App) renderHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()
	size := 0

	ctx, cancel := context.WithTimeout(r.Context(), app.config.Timeouts.Global)
	defer cancel()
	span := trace.SpanFromContext(ctx)
	uuid := util.GetUUID(ctx)

	partiallyFailed := false
	toLog := carbonapipb.NewAccessLogDetails(r, "render", &app.config)
	span.SetAttribute("graphite.username", toLog.Username)

	logAsError := false
	defer func() {
		//TODO: cleanup RenderDurationPerPointExp
		if size > 0 {
			app.prometheusMetrics.RenderDurationPerPointExp.Observe(time.Since(t0).Seconds() * 1000 / float64(size))
		}
		//2xx response code is treated as success
		if toLog.HttpCode/100 == 2 {
			if toLog.TotalMetricCount < int64(app.config.MaxBatchSize) {
				app.prometheusMetrics.RenderDurationExpSimple.Observe(time.Since(t0).Seconds())
				app.prometheusMetrics.RenderDurationLinSimple.Observe(time.Since(t0).Seconds())
			} else {
				app.prometheusMetrics.RenderDurationExpComplex.Observe(time.Since(t0).Seconds())
			}
		}
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()

	form, err := app.renderHandlerProcessForm(r, &toLog, logger)
	if err != nil {
		writeError(uuid, r, w, http.StatusBadRequest, err.Error(), form.format, &toLog, span)
		logAsError = true
		return
	}

	if form.from32 >= form.until32 {
		var clientErrMsgFmt string
		if form.from32 == form.until32 {
			clientErrMsgFmt = "parameter from=%s has the same value as parameter until=%s. Result time range is empty"
		} else {
			clientErrMsgFmt = "parameter from=%s greater than parameter until=%s. Result time range is empty"
		}
		clientErrMsg := fmt.Sprintf(clientErrMsgFmt, form.from, form.until)
		writeError(uuid, r, w, http.StatusBadRequest, clientErrMsg, form.format, &toLog, span)
		toLog.HttpCode = http.StatusBadRequest
		toLog.Reason = "invalid empty time range"
		logAsError = true
		return
	}

	if form.useCache {
		tc := time.Now()
		response, cacheErr := app.queryCache.Get(form.cacheKey)
		td := time.Since(tc).Nanoseconds()
		apiMetrics.RenderCacheOverheadNS.Add(td)

		toLog.CarbonzipperResponseSizeBytes = 0
		toLog.CarbonapiResponseSizeBytes = int64(len(response))

		if cacheErr == nil {
			apiMetrics.RequestCacheHits.Add(1)
			writeErr := writeResponse(ctx, w, response, form.format, form.jsonp)
			if writeErr != nil {
				logAsError = true
			}
			toLog.FromCache = true
			span.SetAttribute("from_cache", true)
			toLog.HttpCode = http.StatusOK
			return
		}
		apiMetrics.RequestCacheMisses.Add(1)
	}
	span.SetAttribute("from_cache", false)

	metricMap := make(map[parser.MetricRequest][]*types.MetricData)

	tracer := span.Tracer()
	var results []*types.MetricData
	for targetIdx := 0; targetIdx < len(form.targets); targetIdx++ {
		target := form.targets[targetIdx]
		targetCtx, targetSpan := tracer.Start(ctx, "carbonapi render", trace.WithAttributes(
			kv.String("graphite.target", target),
		))
		exp, e, parseErr := parser.ParseExpr(target)
		if parseErr != nil || e != "" {
			msg := buildParseErrorString(target, e, parseErr)
			writeError(uuid, r, w, http.StatusBadRequest, msg, form.format, &toLog, span)
			logAsError = true
			return
		}
		targetSpan.AddEvent(targetCtx, "parsed expression")

		getTargetData := func(ctx context.Context, exp parser.Expr, from, until int32, metricMap map[parser.MetricRequest][]*types.MetricData) (error, int) {
			return app.getTargetData(ctx, target, exp, metricMap, form.useCache, from, until, &toLog, logger, &partiallyFailed, targetSpan)
		}
		targetSpan.AddEvent(targetCtx, "retrieved target data")

		targetErr, metricSize := app.getTargetData(targetCtx, target, exp, metricMap,
			form.useCache, form.from32, form.until32, &toLog, logger, &partiallyFailed, targetSpan)

		// Continue query execution even though no metric is found in
		// prefetch as there are Graphite query functions that are able
		// to handle no data and users expect proper result returned. Example:
		//
		// 	fallbackSeries(metric.not.exist, constantLine(1))
		//
		// Refrence behaviour in graphite-web: https://github.com/graphite-project/graphite-web/blob/1.1.8/webapp/graphite/render/evaluator.py#L14-L46
		var notFound dataTypes.ErrNotFound
		if targetErr == nil || errors.As(targetErr, &notFound) {
			targetErr = evalExprRender(targetCtx, exp, &results, metricMap, &form, app.config.PrintErrorStackTrace, getTargetData)
		}
		targetSpan.AddEvent(targetCtx, "evaluated expression")

		if targetErr != nil {
			// we can have 3 error types here
			// a) dataTypes.ErrNotFound  > Continue, at the end we check if all errors are 'not found' and we answer with http 404
			// b) parser.ParseError -> Return with this error(like above, but with less details )
			// c) anything else -> continue, answer will be 5xx if all targets have one error
			var parseError parser.ParseError
			switch {
			case errors.As(targetErr, &notFound):
				// When not found, graphite answers with  http 200 and []
			case errors.Is(targetErr, parser.ErrSeriesDoesNotExist):
				// As now carbonapi continues query execution
				// when no metrics are returned, it's possible
				// to have evalExprRender returning this error.
				// carbonapi should continue executing other
				// queries in the API call to keep being backward-compatible.
				//
				// It would be nice to return the error message,
				// but it seems we do not have a way to
				// communicate it to grafana and other users of
				// carbonapi unless failing all the other queries in the same request:
				//
				// * https://github.com/grafana/grafana/blob/v7.5.10/pkg/tsdb/graphite/types.go\#L5-L8
				// * https://github.com/grafana/grafana/blob/v7.5.10/pkg/tsdb/graphite/graphite.go\#L162-L167
			case errors.As(targetErr, &parseError):
				writeError(uuid, r, w, http.StatusBadRequest, targetErr.Error(), form.format, &toLog, span)
				logAsError = true
				return
			case errors.Is(err, context.DeadlineExceeded):
				writeError(uuid, r, w, http.StatusUnprocessableEntity, "request too complex", form.format, &toLog, span)
				logAsError = true
				app.prometheusMetrics.RequestCancel.WithLabelValues(
					"render", ctx.Err().Error(),
				).Inc()
				return
			default:
				writeError(uuid, r, w, http.StatusInternalServerError, targetErr.Error(), form.format, &toLog, span)
				logAsError = true
				return
			}
		}
		size += metricSize
		targetSpan.End()
	}
	toLog.CarbonzipperResponseSizeBytes = int64(size * 8)

	if ctx.Err() != nil {
		app.prometheusMetrics.RequestCancel.WithLabelValues(
			"render", ctx.Err().Error(),
		).Inc()
	}

	body, err := app.renderWriteBody(results, form, r, logger)
	if err != nil {
		writeError(uuid, r, w, http.StatusInternalServerError, err.Error(), form.format, &toLog, span)
		logAsError = true
		return
	}

	writeErr := writeResponse(ctx, w, body, form.format, form.jsonp)
	if writeErr != nil {
		toLog.HttpCode = 499
	}
	if len(results) != 0 {
		tc := time.Now()
		// TODO (grzkv): Timeout is passed as "expire" argument.
		// Looks like things are mixed.
		app.queryCache.Set(form.cacheKey, body, form.cacheTimeout)
		td := time.Since(tc).Nanoseconds()
		apiMetrics.RenderCacheOverheadNS.Add(td)
	}

	if partiallyFailed {
		app.prometheusMetrics.RenderPartialFail.Inc()
	}
	toLog.HttpCode = http.StatusOK
}

func writeError(uuid string,
	r *http.Request, w http.ResponseWriter,
	code int, s string, format string,
	accessLogDetails *carbonapipb.AccessLogDetails,
	span trace.Span) {
	// TODO (grzkv) Maybe add SVG format handling

	accessLogDetails.HttpCode = int32(code)
	accessLogDetails.Reason = s
	span.SetAttribute("error", true)
	span.SetAttribute("error.message", s)
	if format == pngFormat {
		shortErrStr := http.StatusText(code) + " (" + strconv.Itoa(code) + ")"
		w.Header().Set("X-Carbonapi-UUID", uuid)
		w.Header().Set("Content-Type", contentTypePNG)
		w.WriteHeader(code)
		body, pngErr := png.MarshalPNGRequestErr(r, shortErrStr, "default")
		if pngErr != nil {
			// #pass
		}
		_, err := w.Write(body)
		if err != nil {
			accessLogDetails.Reason += " 499"
		}
	} else {
		http.Error(w, http.StatusText(code)+" ("+strconv.Itoa(code)+") Details: "+s, code)
	}
}

func evalExprRender(ctx context.Context, exp parser.Expr, res *([]*types.MetricData),
	metricMap map[parser.MetricRequest][]*types.MetricData,
	form *renderForm, printErrorStackTrace bool, getTargetData interfaces.GetTargetData) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during expr eval: %s", r)
			if printErrorStackTrace {
				debug.PrintStack()
			}
		}
	}()

	exprs, err := expr.EvalExpr(ctx, exp, form.from32, form.until32, metricMap, getTargetData)
	if err != nil {
		return err
	}

	*res = append(*res, exprs...)

	return nil
}

func (app *App) getTargetData(ctx context.Context, target string, exp parser.Expr,
	metricMap map[parser.MetricRequest][]*types.MetricData,
	useCache bool, from, until int32,
	toLog *carbonapipb.AccessLogDetails, lg *zap.Logger, partFail *bool,
	span trace.Span) (error, int) {

	size := 0
	metrics := 0
	var targetMetricFetches []parser.MetricRequest
	var metricErrs []error

	for _, m := range exp.Metrics() {
		mfetch := m
		mfetch.From += from
		mfetch.Until += until

		targetMetricFetches = append(targetMetricFetches, mfetch)
		if _, ok := metricMap[mfetch]; ok {
			// already fetched this metric for this request
			continue
		}

		// This _sometimes_ sends a *find* request
		renderRequests, err := app.getRenderRequests(ctx, m, useCache, toLog)
		if err != nil {
			metricErrs = append(metricErrs, err)
			continue
		} else if len(renderRequests) == 0 {
			metricErrs = append(metricErrs, dataTypes.ErrMetricsNotFound)
			continue
		}
		renderRequestContext := ctx
		subrequestCount := len(renderRequests)
		if subrequestCount > 1 {
			renderRequestContext = util.WithPriority(ctx, subrequestCount)
		}
		// TODO(dgryski): group the render requests into batches
		rch := make(chan renderResponse, len(renderRequests))
		for _, m := range renderRequests {
			// TODO (grzkv) Refactor to enable premature cancel
			go app.sendRenderRequest(renderRequestContext, rch, m, mfetch.From, mfetch.Until, toLog)
		}

		errs := make([]error, 0)
		for i := 0; i < len(renderRequests); i++ {
			resp := <-rch
			if resp.error != nil {
				errs = append(errs, resp.error)
				continue
			}

			for _, r := range resp.data {
				metrics++
				size += len(r.Values) // close enough
				metricMap[mfetch] = append(metricMap[mfetch], r)
			}
		}
		close(rch)
		// We have to check it here because we don't want to return before closing rch
		select {
		case <-ctx.Done():
			return ctx.Err(), 0
		default:
		}

		metricErr, metricErrStr := optimistFanIn(errs, len(renderRequests), "requests")
		*partFail = (*partFail) || (metricErrStr != "")
		if metricErr != nil {
			metricErrs = append(metricErrs, metricErr)
		}

		expr.SortMetrics(metricMap[mfetch], mfetch)
	} // range exp.Metrics

	span.SetAttribute("graphite.metrics", metrics)
	span.SetAttribute("graphite.datapoints", size)

	targetErr, targetErrStr := optimistFanIn(metricErrs, len(exp.Metrics()), "metrics")
	*partFail = *partFail || (targetErrStr != "")

	logStepTimeMismatch(targetMetricFetches, metricMap, lg, target)
	span.SetAttribute("graphite.metric_errors", targetErrStr)

	return targetErr, size
}

// returns non-nil error when errors result in an error
// returns non-empty string when there are *some* errors, even when total err is nil
// returned string can be used to indicate partial failure
func optimistFanIn(errs []error, n int, subj string) (error, string) {
	nErrs := len(errs)
	if nErrs == 0 {
		return nil, ""
	}

	// everything failed.
	// If all the failures are not-founds, it's a not-found
	allErrorsNotFound := true
	errStr := ""
	for _, e := range errs {
		var notFound dataTypes.ErrNotFound
		errStr = errStr + e.Error() + ", "
		if !errors.As(e, &notFound) {
			allErrorsNotFound = false
		}
	}

	if len(errStr) > 200 {
		errStr = errStr[0:200]
	}

	if nErrs < n {
		return nil, errStr
	}

	if allErrorsNotFound {
		return dataTypes.ErrNotFound("all " + subj +
			" not found; merged errs: (" + errStr + ")"), errStr
	}

	return errors.New("all " + subj +
		" failed with mixed errrors; merged errs: (" + errStr + ")"), errStr
}

func (app *App) sendRenderRequest(ctx context.Context, ch chan<- renderResponse,
	path string, from, until int32, toLog *carbonapipb.AccessLogDetails) {

	apiMetrics.RenderRequests.Add(1)
	atomic.AddInt64(&toLog.ZipperRequests, 1)

	request := dataTypes.NewRenderRequest([]string{path}, from, until)
	metrics, err := app.backend.Render(ctx, request)

	// time in queue is converted to ms
	app.prometheusMetrics.TimeInQueueExp.Observe(float64(request.Trace.Report()[2]) / 1000 / 1000)
	app.prometheusMetrics.TimeInQueueLin.Observe(float64(request.Trace.Report()[2]) / 1000 / 1000)

	metricData := make([]*types.MetricData, 0)
	for i := range metrics {
		metricData = append(metricData, &types.MetricData{
			Metric: metrics[i],
		})
	}

	ch <- renderResponse{
		data:  metricData,
		error: err,
	}
}

type renderForm struct {
	targets      []string
	from         string
	until        string
	format       string
	template     string
	useCache     bool
	from32       int32
	until32      int32
	jsonp        string
	cacheKey     string
	cacheTimeout int32
	qtz          string
}

func (app *App) renderHandlerProcessForm(r *http.Request, accessLogDetails *carbonapipb.AccessLogDetails, logger *zap.Logger) (renderForm, error) {
	var res renderForm

	err := r.ParseForm()
	if err != nil {
		return res, err
	}

	res.targets = r.Form["target"]
	res.from = r.FormValue("from")
	res.until = r.FormValue("until")
	res.format = r.FormValue("format")
	res.template = r.FormValue("template")
	res.useCache = !parser.TruthyBool(r.FormValue("noCache"))

	if res.format == jsonFormat {
		// TODO(dgryski): check jsonp only has valid characters
		res.jsonp = r.FormValue("jsonp")
	}

	if res.format == "" && (parser.TruthyBool(r.FormValue("rawData")) || parser.TruthyBool(r.FormValue("rawdata"))) {
		res.format = rawFormat
	}

	if res.format == "" {
		res.format = pngFormat
	}

	res.cacheTimeout = app.config.Cache.DefaultTimeoutSec

	if tstr := r.FormValue("cacheTimeout"); tstr != "" {
		t, err := strconv.ParseInt(tstr, 10, 64)
		if err != nil {
			logger.Error("failed to parse cacheTimeout",
				zap.String("cache_string", tstr),
				zap.Error(err),
			)
		} else {
			res.cacheTimeout = int32(t)
		}
	}

	// make sure the cache key doesn't say noCache, because it will never hit
	r.Form.Del("noCache")

	// jsonp callback names are frequently autogenerated and hurt our cache
	r.Form.Del("jsonp")

	// Strip some cache-busters.  If you don't want to cache, use noCache=1
	r.Form.Del("_salt")
	r.Form.Del("_ts")
	r.Form.Del("_t") // Used by jquery.graphite.js

	res.cacheKey = r.Form.Encode()

	// normalize from and until values
	res.qtz = r.FormValue("tz")
	var errFrom, errUntil error
	res.from32, errFrom = date.DateParamToEpoch(res.from, res.qtz, timeNow().Add(-24*time.Hour).Unix(), app.defaultTimeZone)
	res.until32, errUntil = date.DateParamToEpoch(res.until, res.qtz, timeNow().Unix(), app.defaultTimeZone)

	accessLogDetails.UseCache = res.useCache
	accessLogDetails.FromRaw = res.from
	accessLogDetails.From = res.from32
	accessLogDetails.UntilRaw = res.until
	accessLogDetails.Until = res.until32
	accessLogDetails.Tz = res.qtz
	accessLogDetails.CacheTimeout = res.cacheTimeout
	accessLogDetails.Format = res.format
	accessLogDetails.Targets = res.targets

	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(
		kv.Bool("graphite.useCache", res.useCache),
		kv.String("graphite.fromRaw", res.from),
		kv.Int32("graphite.from", res.from32),
		kv.String("graphite.untilRaw", res.until),
		kv.Int32("graphite.until", res.until32),
		kv.String("graphite.tz", res.qtz),
		kv.Int32("graphite.cacheTimeout", res.cacheTimeout),
		kv.String("graphite.format", res.format),
	)

	if errFrom != nil || errUntil != nil {
		errFmt := "%s, invalid parameter %s=%s"
		if errFrom != nil {
			return res, fmt.Errorf(errFmt, errFrom.Error(), "from", res.from)
		}
		return res, fmt.Errorf(errFmt, errUntil.Error(), "until", res.until)
	}

	return res, nil
}

func (app *App) renderWriteBody(results []*types.MetricData, form renderForm, r *http.Request, logger *zap.Logger) ([]byte, error) {
	var body []byte
	var err error

	switch form.format {
	case jsonFormat:
		if maxDataPoints, _ := strconv.Atoi(r.FormValue("maxDataPoints")); maxDataPoints != 0 {
			results = types.ConsolidateJSON(maxDataPoints, results)
		}

		body = types.MarshalJSON(results)
	case protobufFormat, protobuf3Format:
		body, err = types.MarshalProtobuf(results)
		if err != nil {
			return body, fmt.Errorf("error while marshalling protobuf: %w", err)
		}
	case rawFormat:
		body = types.MarshalRaw(results)
	case csvFormat:
		tz := app.defaultTimeZone
		if form.qtz != "" {
			var z *time.Location
			z, err = time.LoadLocation(form.qtz)
			if err != nil {
				logger.Warn("Invalid time zone",
					zap.String("tz", form.qtz),
				)
			} else {
				tz = z
			}
		}
		body = types.MarshalCSV(results, tz)
	case pickleFormat:
		body, err = types.MarshalPickle(results)
		if err != nil {
			return body, fmt.Errorf("error while marshalling pickle: %w", err)
		}
	case pngFormat:
		body, err = png.MarshalPNGRequest(r, results, form.template)
		if err != nil {
			return body, fmt.Errorf("error while marshalling PNG: %w", err)
		}
	case svgFormat:
		body, err = png.MarshalSVGRequest(r, results, form.template)
		if err != nil {
			return body, fmt.Errorf("error while marshalling SVG: %w", err)
		}
	}

	return body, nil
}

func (app *App) sendGlobs(glob dataTypes.Matches) bool {
	if app.config.AlwaysSendGlobsAsIs {
		return true
	}

	return app.config.SendGlobsAsIs && len(glob.Matches) < app.config.MaxBatchSize
}

func (app *App) resolveGlobsFromCache(metric string) (dataTypes.Matches, error) {
	tc := time.Now()
	blob, err := app.findCache.Get(metric)
	td := time.Since(tc).Nanoseconds()
	apiMetrics.FindCacheOverheadNS.Add(td)

	if err != nil {
		return dataTypes.Matches{}, err
	}

	matches, err := carbonapi_v2.FindDecoder(blob)
	if err != nil {
		return matches, err
	}

	apiMetrics.FindCacheHits.Add(1)

	return matches, nil
}

func (app *App) resolveGlobs(ctx context.Context, metric string, useCache bool, accessLogDetails *carbonapipb.AccessLogDetails) (dataTypes.Matches, bool, error) {
	if useCache {
		matches, err := app.resolveGlobsFromCache(metric)
		if err == nil {
			return matches, true, nil
		}
	}

	apiMetrics.FindCacheMisses.Add(1)
	apiMetrics.FindRequests.Add(1)
	accessLogDetails.ZipperRequests++

	request := dataTypes.NewFindRequest(metric)
	request.IncCall()
	matches, err := app.backend.Find(ctx, request)
	if err != nil {
		return matches, false, err
	}

	blob, err := carbonapi_v2.FindEncoder(matches)
	if err == nil {
		tc := time.Now()
		app.findCache.Set(metric, blob, app.config.Cache.DefaultTimeoutSec)
		td := time.Since(tc).Nanoseconds()
		apiMetrics.FindCacheOverheadNS.Add(td)
	}

	return matches, false, nil
}

func (app *App) getRenderRequests(ctx context.Context, m parser.MetricRequest, useCache bool,
	toLog *carbonapipb.AccessLogDetails) ([]string, error) {
	if app.config.AlwaysSendGlobsAsIs {
		return []string{m.Metric}, nil
	}
	if !strings.ContainsAny(m.Metric, "*{") {
		return []string{m.Metric}, nil
	}

	glob, _, err := app.resolveGlobs(ctx, m.Metric, useCache, toLog)
	toLog.TotalMetricCount += int64(len(glob.Matches))
	if err != nil {
		return nil, err
	}

	if app.sendGlobs(glob) {
		return []string{m.Metric}, nil
	}

	toLog.SendGlobs = false
	renderRequests := make([]string, 0, len(glob.Matches))
	for _, m := range glob.Matches {
		if m.IsLeaf {
			renderRequests = append(renderRequests, m.Path)
		}
	}

	return renderRequests, nil
}

func (app *App) findHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), app.config.Timeouts.Global)
	defer cancel()
	span := trace.SpanFromContext(ctx)
	uuid := util.GetUUID(ctx)

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()

	format := r.FormValue("format")
	jsonp := r.FormValue("jsonp")
	query := r.FormValue("query")
	useCache := !parser.TruthyBool(r.FormValue("noCache"))

	toLog := carbonapipb.NewAccessLogDetails(r, "find", &app.config)
	toLog.Targets = []string{query}
	span.SetAttributes(
		kv.String("grahite.target", query),
		kv.String("graphite.username", toLog.Username),
	)

	logAsError := false
	defer func() {
		if toLog.HttpCode/100 == 2 {
			if toLog.TotalMetricCount < int64(app.config.MaxBatchSize) {
				app.prometheusMetrics.FindDurationLinSimple.Observe(time.Since(t0).Seconds())
			} else {
				app.prometheusMetrics.FindDurationLinComplex.Observe(time.Since(t0).Seconds())
			}
		}
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	if format == completerFormat {
		query = getCompleterQuery(query)
	}

	if format == "" {
		format = treejsonFormat
	}

	if query == "" {
		writeError(uuid, r, w, http.StatusBadRequest, "missing parameter `query`", "", &toLog, span)
		logAsError = true
		return
	}
	span.SetAttribute("graphite.format", format)
	metrics, fromCache, err := app.resolveGlobs(ctx, query, useCache, &toLog)
	toLog.FromCache = fromCache
	if err == nil {
		toLog.TotalMetricCount = int64(len(metrics.Matches))
		span.SetAttribute("graphite.total_metric_count", toLog.TotalMetricCount)
	} else {
		logger.Warn("zipper returned error in find request",
			zap.String("uuid", util.GetUUID(ctx)),
			zap.Error(err),
		)
		var notFound dataTypes.ErrNotFound

		switch {
		case errors.As(err, &notFound):
			// graphite-web 0.9.12 needs to get a 200 OK response with an empty
			// body to be happy with its life, so we can't 404 a /metrics/find
			// request that finds nothing. We are however interested in knowing
			// that we found nothing on the monitoring side, so we claim we
			// returned a 404 code to Prometheus.
			app.prometheusMetrics.FindNotFound.Inc()
		case errors.Is(err, context.DeadlineExceeded):
			writeError(uuid, r, w, http.StatusUnprocessableEntity, "request too complex", "", &toLog, span)
			apiMetrics.Errors.Add(1)
			logAsError = true
			return
		default:
			writeError(uuid, r, w, http.StatusUnprocessableEntity, err.Error(), "", &toLog, span)
			apiMetrics.Errors.Add(1)
			logAsError = true
			return
		}
	}

	if ctx.Err() != nil {
		app.prometheusMetrics.RequestCancel.WithLabelValues(
			"find", ctx.Err().Error(),
		).Inc()
	}

	var contentType string
	var blob []byte
	switch format {
	case protobufFormat, protobuf3Format:
		contentType = contentTypeProtobuf
		blob, err = carbonapi_v2.FindEncoder(metrics)
	case treejsonFormat, jsonFormat:
		contentType = contentTypeJSON
		blob, err = ourJson.FindEncoder(metrics)
	case "", pickleFormat:
		contentType = contentTypePickle
		if app.config.GraphiteWeb09Compatibility {
			blob, err = pickle.FindEncoderV0_9(metrics)
		} else {
			blob, err = pickle.FindEncoderV1_0(metrics)
		}
	case rawFormat:
		blob = findList(metrics)
		contentType = rawFormat
	case completerFormat:
		blob, err = findCompleter(metrics)
		contentType = jsonFormat
	default:
		err = fmt.Errorf("Unknown format %s", format)
	}

	if err != nil {
		writeError(uuid, r, w, http.StatusInternalServerError, err.Error(), "", &toLog, span)
		logAsError = true
		return
	}

	if contentType == jsonFormat && jsonp != "" {
		w.Header().Set("Content-Type", contentTypeJavaScript)
		if _, writeErr := w.Write([]byte(jsonp)); writeErr != nil {
			toLog.HttpCode = 499
			return
		}
		if _, writeErr := w.Write([]byte{'('}); writeErr != nil {
			toLog.HttpCode = 499
			return
		}
		if _, writeErr := w.Write(blob); writeErr != nil {
			toLog.HttpCode = 499
			return
		}
		if _, writeErr := w.Write([]byte{')'}); writeErr != nil {
			toLog.HttpCode = 499
			return
		}
	} else {
		w.Header().Set("Content-Type", contentType)
		if _, writeErr := w.Write(blob); writeErr != nil {
			toLog.HttpCode = 499
			return
		}
	}

	toLog.HttpCode = http.StatusOK
}

func getCompleterQuery(query string) string {
	var replacer = strings.NewReplacer("/", ".")
	query = replacer.Replace(query)
	if query == "" || query == "/" || query == "." {
		query = ".*"
	} else {
		query += "*"
	}
	return query
}

type completer struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	IsLeaf string `json:"is_leaf"`
}

func findCompleter(globs dataTypes.Matches) ([]byte, error) {
	var b bytes.Buffer

	var complete = make([]completer, 0)

	for _, g := range globs.Matches {
		path := g.Path
		if !g.IsLeaf && path[len(path)-1:] != "." {
			path = g.Path + "."
		}
		c := completer{
			Path: path,
		}

		if g.IsLeaf {
			c.IsLeaf = "1"
		} else {
			c.IsLeaf = "0"
		}

		i := strings.LastIndex(c.Path, ".")

		if i != -1 {
			c.Name = c.Path[i+1:]
		} else {
			c.Name = g.Path
		}

		complete = append(complete, c)
	}

	err := json.NewEncoder(&b).Encode(struct {
		Metrics []completer `json:"metrics"`
	}{
		Metrics: complete},
	)
	return b.Bytes(), err
}

func findList(globs dataTypes.Matches) []byte {
	var b bytes.Buffer

	for _, g := range globs.Matches {

		var dot string
		// make sure non-leaves end in one dot
		if !g.IsLeaf && !strings.HasSuffix(g.Path, ".") {
			dot = "."
		}

		fmt.Fprintln(&b, g.Path+dot)
	}

	return b.Bytes()
}

func (app *App) infoHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), app.config.Timeouts.Global)
	defer cancel()

	format := r.FormValue("format")

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()

	if format == "" {
		format = jsonFormat
	}

	toLog := carbonapipb.NewAccessLogDetails(r, "info", &app.config)
	toLog.Format = format

	logAsError := false
	defer func() {
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	query := r.FormValue("target")
	if query == "" {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		toLog.HttpCode = http.StatusBadRequest
		toLog.Reason = "no target specified"
		logAsError = true
		return
	}

	request := dataTypes.NewInfoRequest(query)
	request.IncCall()
	infos, err := app.backend.Info(ctx, request)
	if err != nil {
		var notFound dataTypes.ErrNotFound
		if errors.As(err, &notFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			toLog.HttpCode = http.StatusNotFound
			toLog.Reason = "info not found"
			logAsError = true
			return
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		toLog.HttpCode = http.StatusInternalServerError
		toLog.Reason = err.Error()
		logAsError = true
		return
	}

	var b []byte
	var contentType string
	switch format {
	case jsonFormat:
		contentType = contentTypeJSON
		b, err = ourJson.InfoEncoder(infos)
	case protobufFormat, protobuf3Format:
		contentType = contentTypeProtobuf
		b, err = carbonapi_v2.InfoEncoder(infos)
	default:
		err = fmt.Errorf("unknown format %v", format)
	}

	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		toLog.HttpCode = http.StatusInternalServerError
		toLog.Reason = err.Error()
		logAsError = true
		return
	}

	w.Header().Set("Content-Type", contentType)
	_, writeErr := w.Write(b)
	toLog.Runtime = time.Since(t0).Seconds()
	if writeErr != nil {
		toLog.HttpCode = 499
		return
	}

	toLog.HttpCode = http.StatusOK
}

func (app *App) lbcheckHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()
	defer func() {
		apiMetrics.Responses.Add(1)
		app.prometheusMetrics.Responses.WithLabelValues(strconv.Itoa(http.StatusOK), "lbcheck", "false").Inc()
	}()

	_, writeErr := w.Write([]byte("Ok\n"))

	toLog := carbonapipb.NewAccessLogDetails(r, "lbcheck", &app.config)
	toLog.Runtime = time.Since(t0).Seconds()
	toLog.HttpCode = http.StatusOK
	if writeErr != nil {
		toLog.HttpCode = 499
	}

	fields, err := toLog.GetLogFields()
	if err != nil {
		logger.Error("could not marshal access log details", zap.Error(err))
	}
	logger.Info("request served", fields...)
}

func (app *App) versionHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()
	defer func() {
		apiMetrics.Responses.Add(1)
		app.prometheusMetrics.Responses.WithLabelValues(strconv.Itoa(http.StatusOK), "version", "false").Inc()
	}()
	// Use a specific version of graphite for grafana
	// This handler is queried by grafana, and if needed, an override can be provided
	if app.config.GraphiteVersionForGrafana != "" {
		_, err := w.Write([]byte(app.config.GraphiteVersionForGrafana))
		if err != nil {
			// #pass, do not log
		}
		return
	}
	toLog := carbonapipb.NewAccessLogDetails(r, "version", &app.config)
	toLog.HttpCode = http.StatusOK

	if app.config.GraphiteWeb09Compatibility {
		if _, err := w.Write([]byte("0.9.15\n")); err != nil {
			toLog.HttpCode = 499
		}
	} else {
		if _, err := w.Write([]byte("1.0.0\n")); err != nil {
			toLog.HttpCode = 499
		}
	}

	toLog.Runtime = time.Since(t0).Seconds()

	fields, err := toLog.GetLogFields()
	if err != nil {
		logger.Error("could not marshal access log details", zap.Error(err))
	}
	logger.Info("request served", fields...)
}

func (app *App) functionsHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	// TODO: Implement helper for specific functions
	t0 := time.Now()

	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()

	toLog := carbonapipb.NewAccessLogDetails(r, "functions", &app.config)

	logAsError := false
	defer func() {
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	err := r.ParseForm()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest)+": "+err.Error(), http.StatusBadRequest)
		toLog.HttpCode = http.StatusBadRequest
		toLog.Reason = err.Error()
		logAsError = true
		return
	}

	grouped := false
	nativeOnly := false
	groupedStr := r.FormValue("grouped")
	prettyStr := r.FormValue("pretty")
	nativeOnlyStr := r.FormValue("nativeOnly")
	var marshaler func(interface{}) ([]byte, error)

	if groupedStr == "1" {
		grouped = true
	}

	if prettyStr == "1" {
		marshaler = func(v interface{}) ([]byte, error) {
			return json.MarshalIndent(v, "", "\t")
		}
	} else {
		marshaler = json.Marshal
	}

	if nativeOnlyStr == "1" {
		nativeOnly = true
	}

	path := strings.Split(r.URL.EscapedPath(), "/")
	function := ""
	if len(path) >= 3 {
		function = path[2]
	}

	var b []byte
	if !nativeOnly {
		metadata.FunctionMD.RLock()
		if function != "" {
			b, err = marshaler(metadata.FunctionMD.Descriptions[function])
		} else if grouped {
			b, err = marshaler(metadata.FunctionMD.DescriptionsGrouped)
		} else {
			b, err = marshaler(metadata.FunctionMD.Descriptions)
		}
		metadata.FunctionMD.RUnlock()
	} else {
		metadata.FunctionMD.RLock()
		if function != "" {
			if !metadata.FunctionMD.Descriptions[function].Proxied {
				b, err = marshaler(metadata.FunctionMD.Descriptions[function])
			} else {
				err = fmt.Errorf("%v is proxied to graphite-web and nativeOnly was specified", function)
			}
		} else if grouped {
			descGrouped := make(map[string]map[string]types.FunctionDescription)
			for groupName, description := range metadata.FunctionMD.DescriptionsGrouped {
				desc := make(map[string]types.FunctionDescription)
				for f, d := range description {
					if d.Proxied {
						continue
					}
					desc[f] = d
				}
				if len(desc) > 0 {
					descGrouped[groupName] = desc
				}
			}
			b, err = marshaler(descGrouped)
		} else {
			desc := make(map[string]types.FunctionDescription)
			for f, d := range metadata.FunctionMD.Descriptions {
				if d.Proxied {
					continue
				}
				desc[f] = d
			}
			b, err = marshaler(desc)
		}
		metadata.FunctionMD.RUnlock()
	}

	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		toLog.HttpCode = http.StatusInternalServerError
		toLog.Reason = err.Error()
		logAsError = true
		return
	}

	_, err = w.Write(b)
	toLog.Runtime = time.Since(t0).Seconds()
	toLog.HttpCode = http.StatusOK
	if err != nil {
		toLog.HttpCode = 499
	}
}

// Add block rules on the basis of headers to block certain requests
// To be used to block read abusers
// The rules are added(appended) in the block headers config file
// Returns failure if handler is invoked and config entry is missing
// Otherwise, it creates the config file with the rule
func (app *App) blockHeaders(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()

	apiMetrics.Requests.Add(1)

	toLog := carbonapipb.NewAccessLogDetails(r, "blockHeaders", &app.config)

	logAsError := false
	defer func() {
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	w.Header().Set("Content-Type", contentTypeJSON)

	failResponse := []byte(`{"success":"false"}`)
	if !app.requestBlocker.AddNewRules(r.URL.Query()) {
		w.WriteHeader(http.StatusBadRequest)
		toLog.HttpCode = http.StatusBadRequest
		if _, err := w.Write(failResponse); err != nil {
			toLog.HttpCode = 499
		}
		return
	}
	_, err := w.Write([]byte(`{"success":"true"}`))

	toLog.HttpCode = http.StatusOK
	if err != nil {
		toLog.HttpCode = 499
	}
}

// It deletes the block headers config file
// Use it to remove all blocking rules, or to restart adding rules
// from scratch
func (app *App) unblockHeaders(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	t0 := time.Now()
	apiMetrics.Requests.Add(1)
	toLog := carbonapipb.NewAccessLogDetails(r, "unblockHeaders", &app.config)

	logAsError := false
	defer func() {
		app.deferredAccessLogging(logger, r, &toLog, t0, logAsError)
	}()

	w.Header().Set("Content-Type", contentTypeJSON)
	err := app.requestBlocker.Unblock()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		toLog.HttpCode = http.StatusBadRequest
		if _, writeErr := w.Write([]byte(`{"success":"false"}`)); writeErr != nil {
			toLog.HttpCode = 499
		}
		return
	}
	_, err = w.Write([]byte(`{"success":"true"}`))
	toLog.HttpCode = http.StatusOK
	if err != nil {
		toLog.HttpCode = 499
	}

}

func logStepTimeMismatch(targetMetricFetches []parser.MetricRequest, metricMap map[parser.MetricRequest][]*types.MetricData, logger *zap.Logger, target string) {
	var defaultStepTime int32 = -1
	for _, mfetch := range targetMetricFetches {
		values := metricMap[mfetch]
		if len(values) == 0 {
			continue
		}
		if defaultStepTime <= 0 {
			defaultStepTime = values[0].StepTime
		}
		if !isStepTimeMatching(values[:], defaultStepTime) {
			logger.Info("metrics with differing resolution", zap.Any("target", target))
			return
		}
	}
}

func isStepTimeMatching(value []*types.MetricData, defaultStepTime int32) bool {
	for _, val := range value {
		if defaultStepTime != val.StepTime {
			return false
		}
	}
	return true
}

var usageMsg = []byte(`
supported requests:
	/render/?target=
	/metrics/find/?query=
	/info/?target=
	/functions/
	/tags/autoComplete/tags
`)

func (app *App) usageHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()
	defer func() {
		apiMetrics.Responses.Add(1)
		app.prometheusMetrics.Responses.WithLabelValues(strconv.Itoa(http.StatusOK), "usage", "false").Inc()
	}()
	toLog := carbonapipb.NewAccessLogDetails(r, "usage", &app.config)
	toLog.HttpCode = http.StatusOK
	_, err := w.Write(usageMsg)
	if err != nil {
		toLog.HttpCode = 499
	}
}

//TODO : Fix this handler if and when tag support is added
// This responds to grafana's tag requests, which were falling through to the usageHandler,
// preventing a random, garbage list of tags (constructed from usageMsg) being added to the metrics list
func (app *App) tagsHandler(w http.ResponseWriter, r *http.Request, logger *zap.Logger) {
	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()
	defer func() {
		apiMetrics.Responses.Add(1)
		app.prometheusMetrics.Responses.WithLabelValues(strconv.Itoa(http.StatusOK), "tags", "false").Inc()
	}()
}

func (app *App) debugVersionHandler(w http.ResponseWriter, r *http.Request) {
	apiMetrics.Requests.Add(1)
	app.prometheusMetrics.Requests.Inc()
	defer func() {
		apiMetrics.Responses.Add(1)
		app.prometheusMetrics.Responses.WithLabelValues(strconv.Itoa(http.StatusOK), "debugversion", "false").Inc()
	}()

	fmt.Fprintf(w, "GIT_TAG: %s\n", BuildVersion)
}

func buildParseErrorString(target, e string, err error) string {
	msg := fmt.Sprintf("%s\n\n%-20s: %s\n", http.StatusText(http.StatusBadRequest), "Target", target)
	if err != nil {
		msg += fmt.Sprintf("%-20s: %s\n", "Error", err.Error())
	}
	if e != "" {
		msg += fmt.Sprintf("%-20s: %s\n%-20s: %s\n",
			"Parsed so far", target[0:len(target)-len(e)],
			"Could not parse", e)
	}
	return msg
}
