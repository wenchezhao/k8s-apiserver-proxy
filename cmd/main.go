package main

import (
	"github.com/gin-gonic/gin"
	resourcemanage "k8s-apiserver-proxy/pkg/resourcemanage/handle"
)

func main() {
	router := gin.New()
	k8sApiProxy := router.Group("")
	{
		proxyHandler := resourcemanage.NewProxyHandler(true)
		k8sApiProxy.Any("*url", proxyHandler.ProxyHandle)
	}
	//router.Run()
	router.RunTLS(":8080",,)
}
