package main

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

func clientgoscheme() *runtime.Scheme  { return scheme.Scheme }
func ctrlGetConfigOrDie() *rest.Config { return ctrl.GetConfigOrDie() }
