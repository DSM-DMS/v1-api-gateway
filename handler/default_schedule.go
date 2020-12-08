package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"gateway/entity"
	scheduleproto "gateway/proto/golang/schedule"
	agenterrors "gateway/tool/consul/agent/errors"
	jwtutil "gateway/tool/jwt"
	code "gateway/utils/code/golang"
	topic "gateway/utils/topic/golang"
	"github.com/eapache/go-resiliency/breaker"
	"github.com/gin-gonic/gin"
	"github.com/micro/go-micro/v2/client"
	"github.com/micro/go-micro/v2/errors"
	"github.com/micro/go-micro/v2/metadata"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/log"
	"github.com/sirupsen/logrus"
	"github.com/uber/jaeger-client-go"
	"net/http"
	"time"
)

func (h *_default) CreateSchedule(c *gin.Context) {
	reqID := c.GetHeader("X-Request-Id")
	topSpan := h.tracer.StartSpan(fmt.Sprintf("%s %s", c.Request.Method, c.FullPath())).SetTag("X-Request-Id", reqID)

	inAdvanceEntry, ok := c.Get("RequestLogEntry")
	entry, ok := inAdvanceEntry.(*logrus.Entry)
	if !ok {
		msg := "unable to get request log entry from middleware"
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "code": 0, "message": msg})
		entry.WithFields(logrus.Fields{"status": http.StatusInternalServerError, "code": 0, "message": msg}).Error()
		topSpan.LogFields(log.Int("status", http.StatusInternalServerError), log.Int("code", 0), log.String("message", msg))
		topSpan.SetTag("status", http.StatusInternalServerError).SetTag("code", 0).Finish()
		return
	}

	// logic handling Unauthorized
	var uuidClaims jwtutil.UUIDClaims
	if ok, claims, _code, msg := h.checkIfAuthenticated(c); ok {
		uuidClaims = claims
	} else {
		c.JSON(http.StatusUnauthorized, gin.H{"status": http.StatusUnauthorized, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": http.StatusUnauthorized, "code": _code, "message": msg}).Info()
		topSpan.LogFields(log.Int("status", http.StatusUnauthorized), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", http.StatusUnauthorized).SetTag("code", _code).Finish()
		return
	}

	// logic handling BadRequest
	var receivedReq entity.CreateScheduleRequest
	if ok, _code, msg := h.checkIfValidRequest(c, &receivedReq); ok {
	} else {
		reqBytes, _ := json.Marshal(receivedReq)
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": http.StatusBadRequest, "code": _code, "message": msg, "request": string(reqBytes)}).Info()
		topSpan.LogFields(log.Int("status", http.StatusBadRequest), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", http.StatusBadRequest).SetTag("code", _code).Finish()
		return
	}
	reqBytes, _ := json.Marshal(receivedReq)

	consulSpan := h.tracer.StartSpan("GetNextServiceNode", opentracing.ChildOf(topSpan.Context()))
	selectedNode, err := h.consulAgent.GetNextServiceNode(topic.ScheduleServiceName)
	if err == nil { consulSpan.SetTag("X-Request-Id", reqID).LogFields(log.Object("SelectedNode", *selectedNode)) }
	consulSpan.LogFields(log.Error(err))
	consulSpan.Finish()

	switch err {
	case nil:
		break
	case agenterrors.AvailableNodeNotExist:
		msg := "available schedule service node is not exist in consul"
		status, _code := http.StatusServiceUnavailable, code.AvailableServiceNotExist
		c.JSON(status, gin.H{"status": status, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
		return
	default:
		msg := fmt.Sprintf("unable to get service node from consul agent, err: %s", err.Error())
		status, _code := http.StatusInternalServerError, 0
		c.JSON(status, gin.H{"status": status, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
		return
	}

	h.mutex.Lock()
	if _, ok := h.breakers[selectedNode.Id]; !ok {
		h.breakers[selectedNode.Id] = breaker.New(h.BreakerCfg.ErrorThreshold, h.BreakerCfg.SuccessThreshold, h.BreakerCfg.Timeout)
	}
	h.mutex.Unlock()

	var rpcResp *scheduleproto.DefaultScheduleResponse
	err = h.breakers[selectedNode.Id].Run(func() (rpcErr error) {
		scheduleSrvSpan := h.tracer.StartSpan("CreateSchedule", opentracing.ChildOf(topSpan.Context()))
		ctxForReq := context.Background()
		ctxForReq = metadata.Set(ctxForReq, "X-Request-Id", reqID)
		ctxForReq = metadata.Set(ctxForReq, "Span-Context", scheduleSrvSpan.Context().(jaeger.SpanContext).String())
		rpcReq := receivedReq.GenerateGRPCRequest()
		rpcReq.Uuid = uuidClaims.UUID
		callOpts := append(h.DefaultCallOpts, client.WithAddress(selectedNode.Address))
		rpcResp, rpcErr = h.scheduleService.CreateSchedule(ctxForReq, rpcReq, callOpts...)
		scheduleSrvSpan.SetTag("X-Request-Id", reqID).LogFields(log.Object("request", rpcReq), log.Object("response", rpcResp), log.Error(rpcErr))
		scheduleSrvSpan.Finish()
		return
	})

	if err == breaker.ErrBreakerOpen {
		msg := fmt.Sprintf("circuit breaker is open (service id: %s, time out: %s)", selectedNode.Id, h.BreakerCfg.Timeout.String())
		status, _code := http.StatusServiceUnavailable, code.CircuitBreakerOpen
		_ = h.consulAgent.FailTTLHealth(selectedNode.Metadata["CheckID"], breaker.ErrBreakerOpen.Error())
		time.AfterFunc(h.BreakerCfg.Timeout, func() { _ = h.consulAgent.PassTTLHealth(selectedNode.Metadata["CheckID"], "close circuit breaker") })
		c.JSON(status, gin.H{"status": status, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
		return
	}

	switch rpcErr := err.(type) {
	case nil:
		break
	case *errors.Error:
		var status, _code int
		var msg string
		switch rpcErr.Code {
		case http.StatusRequestTimeout:
			msg = fmt.Sprintf("request time out for CreateSchedule service, detail: %s", rpcErr.Detail)
			status, _code = http.StatusRequestTimeout, 0
		default:
			msg = fmt.Sprintf("CreateSchedule returns unexpected micro error, code: %d, detail: %s", rpcErr.Code, rpcErr.Detail)
			status, _code = http.StatusInternalServerError, 0
		}
		c.JSON(status, gin.H{"status": status, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
		return
	default:
		status, _code := http.StatusInternalServerError, 0
		msg := fmt.Sprintf("CreateSchedule returns unexpected type of error, err: %s", rpcErr.Error())
		c.JSON(status, gin.H{"status": status, "code": _code, "message": msg})
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
		return
	}

	switch rpcResp.Status {
	case http.StatusCreated:
		status, _code := http.StatusCreated, 0
		msg := "succeed to create new schedule"
		sendResp := gin.H{"status": status, "code": _code, "message": msg, "schedule_uuid": ""}
		c.JSON(status, sendResp)
		respBytes, _ := json.Marshal(sendResp)
		entry.WithFields(logrus.Fields{"status": status, "code": _code, "message": msg, "response": string(respBytes), "request": string(reqBytes)}).Info()
		topSpan.LogFields(log.Int("status", status), log.Int("code", _code), log.String("message", msg))
		topSpan.SetTag("status", status).SetTag("code", _code).Finish()
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusServiceUnavailable:
		c.JSON(int(rpcResp.Status), gin.H{"status": rpcResp.Status, "code": rpcResp.Code, "message": rpcResp.Msg})
		entry.WithFields(logrus.Fields{"status": rpcResp.Status, "code": rpcResp.Code, "message": rpcResp.Msg, "request": string(reqBytes)}).Error()
		topSpan.LogFields(log.Int("status", int(rpcResp.Status)), log.Int("code", int(rpcResp.Code)), log.String("message", rpcResp.Msg))
		topSpan.SetTag("status", rpcResp.Status).SetTag("code", rpcResp.Code).Finish()
	default:
		c.JSON(int(rpcResp.Status), gin.H{"status": rpcResp.Status, "code": rpcResp.Code, "message": rpcResp.Msg})
		entry.WithFields(logrus.Fields{"status": rpcResp.Status, "code": rpcResp.Code, "message": rpcResp.Msg, "request": string(reqBytes)}).Info()
		topSpan.LogFields(log.Int("status", int(rpcResp.Status)), log.Int("code", int(rpcResp.Code)), log.String("message", rpcResp.Msg))
		topSpan.SetTag("status", rpcResp.Status).SetTag("code", rpcResp.Code).Finish()
	}

	return
}
