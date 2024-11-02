package service

import (
	log "github.com/deckhouse/deckhouse/pkg/log"

	_ "github.com/flant/kube-client/klogtolog" // plug in JSON logs adapter
	klog_powered_lib "github.com/flant/kube-client/klogtolog/test/klog-powered-lib"
)

func DoWithCallToKlogPoweredLib() {
	log.Debug("service action")

	klog_powered_lib.ActionWithKlogWarn()
	klog_powered_lib.ActionWithKlogInfo()
	klog_powered_lib.ActionWithKlogError()
}
