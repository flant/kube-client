package service

import (
	log "github.com/sirupsen/logrus"

	_ "github.com/flant/kube-client/klogtologrus" // plug in JSON logs adapter
	klog_powered_lib "github.com/flant/kube-client/klogtologrus/test/klog-powered-lib"
)

func DoWithCallToKlogPoweredLib() {
	log.Trace("service action")

	klog_powered_lib.ActionWithKlogWarn()
	klog_powered_lib.ActionWithKlogInfo()
	klog_powered_lib.ActionWithKlogError()
}
