package gortsplib

import (
	"github.com/inchtime/gortsplib/pkg/formats"
	"github.com/inchtime/gortsplib/pkg/rtcpsender"
)

type serverStreamFormat struct {
	format     formats.Format
	rtcpSender *rtcpsender.RTCPSender
}
