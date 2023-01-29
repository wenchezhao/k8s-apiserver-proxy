/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resourcemanage

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/kubecube-io/kubecube/pkg/clog"
	"io"
	"io/ioutil"
	"k8s-apiserver-proxy/pkg/utils/filter"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httputil"
	"net/url"
	//k8sClient "sigs.k8s.io/descheduler/pkg/descheduler/client"

	//"github.com/kubecube-io/kubecube/pkg/clog"
	"k8s-apiserver-proxy/pkg/conversion"
	"k8s-apiserver-proxy/pkg/k8sclient"
	response "k8s-apiserver-proxy/pkg/resourcemanage"
	//"github.com/kubecube-io/kubecube/pkg/multicluster"
	//"github.com/kubecube-io/kubecube/pkg/utils/constants"
	//"github.com/kubecube-io/kubecube/pkg/utils/errcode"
	//"github.com/kubecube-io/kubecube/pkg/utils/filter"
	//"github.com/kubecube-io/kubecube/pkg/utils/page"
	//requestutil "github.com/kubecube-io/kubecube/pkg/utils/request"
	//"github.com/kubecube-io/kubecube/pkg/utils/response"
	//"github.com/kubecube-io/kubecube/pkg/utils/selector"
	//"github.com/kubecube-io/kubecube/pkg/utils/sort"
)

type ConverterContext struct {
	EnableConvert bool
	RawGvr        *schema.GroupVersionResource
	ConvertedGvr  *schema.GroupVersionResource
	Converter     *conversion.VersionConverter
}

type ProxyHandler struct {
	// enableConvert means proxy handler will convert resources
	enableConvert bool
}

func NewProxyHandler(enableConvert bool) *ProxyHandler {
	return &ProxyHandler{
		enableConvert: enableConvert,
	}
}

// tryVersionConvert try to convert url and request body by given target cluster
func (h *ProxyHandler) tryVersionConvert(config *rest.Config, url string, req *http.Request) (bool, []byte, string, error) {
	if !h.enableConvert {
		return false, nil, "", nil
	}

	_, isNamespaced, gvr, err := conversion.ParseURL(url)
	if err != nil {
		return false, nil, "", err
	}
	// 获取clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, nil, "", err
	}
	// 构建版本转化器
	converter, err := conversion.NewVersionConvertor(clientset.Discovery(), nil)
	if err != nil {
		return false, nil, "", err
	}
	// 检测是否可以进行转换（获取可以转换的版本）
	greetBack, _, recommendVersion, err := converter.GvrGreeting(gvr)
	if err != nil {
		// we just record error and pass through anyway
		clog.Warn(err.Error())
	}
	if greetBack != conversion.IsNeedConvert {
		// pass through anyway if not need convert
		clog.Info("gvr %v greet is %v, pass through", gvr.String(), greetBack)
		return false, nil, "", nil
	}
	if recommendVersion == nil {
		return false, nil, "", nil
	}

	// convert url according to specified gvr at first
	convertedUrl, err := conversion.ConvertURL(url, &schema.GroupVersionResource{Group: recommendVersion.Group, Version: recommendVersion.Version, Resource: gvr.Resource})
	if err != nil {
		return false, nil, "", err
	}

	// we do not need convert body if request not create and update
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return true, nil, convertedUrl, nil
	}

	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return false, nil, "", err
	}
	// decode data into internal version of object
	raw, rawGvr, err := converter.Decode(data, nil, nil)
	if err != nil {
		return false, nil, "", err
	}
	if rawGvr.GroupVersion().String() != gvr.GroupVersion().String() {
		return false, nil, "", fmt.Errorf("gv parse failed with pair(%v~%v)", rawGvr.GroupVersion().String(), gvr.GroupVersion().String())
	}
	// covert internal version object int recommend version object
	out, err := converter.Convert(raw, nil, recommendVersion.GroupVersion())
	if err != nil {
		return false, nil, "", err
	}
	// encode concerted object
	convertedObj, err := converter.Encode(out, recommendVersion.GroupVersion())
	if err != nil {
		return false, nil, "", err
	}

	objMeta, err := meta.Accessor(out)
	if err != nil {
		return false, nil, "", err
	}

	if isNamespaced {
		clog.Info("resource (%v/%v) converted with (%v~%v) when visit cluster", objMeta.GetNamespace(), objMeta.GetName(), gvr.String(), recommendVersion.GroupVersion().WithResource(gvr.Resource))
	} else {
		clog.Info("resource (%v) converted with (%v~%v) when visit cluster", objMeta.GetName(), gvr.String(), recommendVersion.GroupVersion().WithResource(gvr.Resource))
	}

	return true, convertedObj, convertedUrl, nil
}

// ProxyHandle proxy all requests access to k8s, request uri format like below
func (h *ProxyHandler) ProxyHandle(c *gin.Context) {
	// http request params
	//cluster := c.Param("cluster")
	//proxyUrl := c.Param("url")
	proxyUrl := c.Request.URL.Path

	//condition := parseQueryParams(c)
	//// 转换的context
	converterContext := filter.ConverterContext{}
	// 获取连接的config
	config, err := k8sclient.GetKubeConfig("")
	if err != nil {
		response.FailReturn(c, http.StatusBadRequest, err)
		return
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		response.FailReturn(c, http.StatusBadRequest, err)
		return
	}
	_, _, gvr, err := conversion.ParseURL(proxyUrl)
	if err != nil {
		clog.Error(err.Error())
		response.FailReturn(c, http.StatusBadRequest, err)
		return
	}
	// 转化url和request
	needConvert, convertedObj, convertedUrl, err := h.tryVersionConvert(config, proxyUrl, c.Request)
	if err != nil {
		clog.Error(err.Error())
		response.FailReturn(c, http.StatusBadRequest, err)
		return
	}

	// todo: open it when impersonate fixed.
	//allowed, err := belongs.RelationshipDetermine(context.Background(), internalCluster.Client, proxyUrl, username)
	//if err != nil {
	//	clog.Warn(err.Error())
	//} else if !allowed {
	//	response.FailReturn(c, errcode.ForbiddenErr)
	//	return
	//}

	// create director
	director := func(req *http.Request) {
		//labelSelector := selector.ParseLabelSelector(c.Query("selector"))

		uri, err := url.ParseRequestURI(config.Host)
		if err != nil {
			clog.Error("Could not parse host, host: %s , err: %v", config.Host, err)
			response.FailReturn(c, http.StatusBadRequest, err)
		}
		uri.RawQuery = c.Request.URL.RawQuery
		uri.Path = proxyUrl
		req.URL = uri
		req.Host = config.Host

		//err = requestutil.AddFieldManager(req, username)
		//if err != nil {
		//	clog.Error("fail to add fieldManager due to %s", err.Error())
		//}

		if needConvert {
			// replace request body and url if need
			if convertedObj != nil {
				r := bytes.NewReader(convertedObj)
				body := io.NopCloser(r)
				req.Body = body
				req.ContentLength = int64(r.Len())
			}
			req.URL.Path = convertedUrl
		}

		////In order to improve processing efficiency
		////this method converts requests starting with metadata.labels in the selector into k8s labelSelector requests
		//// todo This method can be further optimized and extracted as a function to improve readability
		//if len(labelSelector) > 0 {
		//	labelSelectorQueryString := ""
		//	// Take out the query value in the selector and stitch it into the query field of labelSelector
		//	// for example: selector=metadata.labels.key=value1|value2|value3
		//	// then it should be converted to: key+in+(value1,value2,value3)
		//	for key, value := range labelSelector {
		//		if len(value) < 1 {
		//			continue
		//		}
		//		labelSelectorQueryString += key
		//		labelSelectorQueryString += "+in+("
		//		labelSelectorQueryString += strings.Join(value, ",")
		//		labelSelectorQueryString += ")"
		//		labelSelectorQueryString += ","
		//	}
		//	if len(labelSelectorQueryString) > 0 {
		//		labelSelectorQueryString = strings.TrimRight(labelSelectorQueryString, ",")
		//	}
		//	labelSelectorQueryString = url.PathEscape(labelSelectorQueryString)
		//	// Old query parameters may have the following conditions:
		//	// empty
		//	// has selector: selector=key=value
		//	// has selector and labelSelector: selector=key=value&labelSelector=key=value
		//	// has selector and labelSelector and others: selector=key=value&labelSelector=key=value&fieldSelector=key=value
		//	// so, use & to split it
		//	queryArray := strings.Split(req.URL.RawQuery, "&")
		//	queryString := ""
		//	labelSelectorSet := false
		//	for _, v := range queryArray {
		//		//if it start with labelSelector=, then append converted labelSelector string
		//		if strings.HasPrefix(v, "labelSelector=") {
		//			queryString += v + "," + labelSelectorQueryString
		//			labelSelectorSet = true
		//			// else if url like: selector=key=value&labelSelector, then use converted labelSelector string replace it
		//		} else if strings.HasPrefix(v, "labelSelector") {
		//			queryString += "labelSelector=" + labelSelectorQueryString
		//			labelSelectorSet = true
		//			// else no need to do this
		//		} else {
		//			queryString += v
		//		}
		//		queryString += "&"
		//	}
		//	// If the query parameter does not exist labelSelector
		//	// append converted labelSelector string
		//	if len(queryString) > 0 && labelSelectorSet == false {
		//		queryString += "&labelSelector=" + labelSelectorQueryString
		//	}
		//	req.URL.RawQuery = queryString
		//}
	}

	errorHandler := func(resp http.ResponseWriter, req *http.Request, err error) {
		if err != nil {
			clog.Warn("url %s proxy fail, %v", proxyUrl, err)
			response.FailReturn(c, http.StatusInternalServerError, errors.New("server error"))
			return
		}
	}

	if needConvert {
		// open response filterCondition convert
		_, _, convertedGvr, err := conversion.ParseURL(convertedUrl)
		if err != nil {
			clog.Error(err.Error())
			response.FailReturn(c, http.StatusInternalServerError, errors.New("server error"))
			return
		}

		// 获取clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			clog.Error(err.Error())
			response.FailReturn(c, http.StatusInternalServerError, errors.New("server error"))
			return
		}
		// 构建版本转化器
		converter, err := conversion.NewVersionConvertor(clientset.Discovery(), nil)
		if err != nil {
			clog.Error(err.Error())
			response.FailReturn(c, http.StatusInternalServerError, errors.New("server error"))
			return
		}
		converterContext = filter.ConverterContext{
			EnableConvert: true,
			Converter:     converter,
			ConvertedGvr:  convertedGvr,
			RawGvr:        gvr,
		}
	}

	filter := ResponseFilter{
		Condition: &filter.Condition{
			Exact: make(map[string]sets.String, 0),
			Fuzzy: make(map[string][]string, 0),
		},
		ConverterContext: &converterContext,
	}
	requestProxy := &httputil.ReverseProxy{Director: director, Transport: transport, ModifyResponse: filter.filterResponse, ErrorHandler: errorHandler}

	// trim auth token here
	c.Request.Header.Del("Authorization")

	requestProxy.ServeHTTP(c.Writer, c.Request)
}

//// product match/sort/page to other function
//func Filter(c *gin.Context, object runtime.Object) (*int, error) {
//	condition := parseQueryParams(c)
//	total, err := filter.GetEmptyFilter().FilterObjectList(object, condition)
//	if err != nil {
//		clog.Error("filterCondition userList error, err: %s", err.Error())
//		return nil, err
//	}
//	return &total, nil
//}

// parse request params, include selector, sort and page

//func parseQueryParams(c *gin.Context) *filter.Condition {
//	exact, fuzzy := selector.ParseSelector(c.Query("selector"))
//	limit, offset := page.ParsePage(c.Query("pageSize"), c.Query("pageNum"))
//	sortName, sortOrder, sortFunc := sort.ParseSort(c.Query("sortName"), c.Query("sortOrder"), c.Query("sortFunc"))
//	condition := filter.Condition{
//		Exact:     exact,
//		Fuzzy:     fuzzy,
//		Limit:     limit,
//		Offset:    offset,
//		SortName:  sortName,
//		SortOrder: sortOrder,
//		SortFunc:  sortFunc,
//	}
//	return &condition
//}
