/*
Copyright 2016 The Kubernetes Authors.

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

package watchers

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	"k8s.io/kops/dns-controller/pkg/dns"
	"k8s.io/kops/dns-controller/pkg/util"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	client_extensions "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/typed/extensions/v1beta1"
	"k8s.io/kubernetes/pkg/watch"
)

// IngressController watches for Ingress objects with dns labels
type IngressController struct {
	util.Stoppable
	kubeClient client_extensions.ExtensionsV1beta1Interface
	scope      dns.Scope
}

// newIngressController creates a ingressController
func NewIngressController(kubeClient client_extensions.ExtensionsV1beta1Interface, dns dns.Context) (*IngressController, error) {
	scope, err := dns.CreateScope("ingress")
	if err != nil {
		return nil, fmt.Errorf("error building dns scope: %v", err)
	}
	c := &IngressController{
		kubeClient: kubeClient,
		scope:      scope,
	}

	return c, nil
}

// Run starts the IngressController.
func (c *IngressController) Run() {
	glog.Infof("starting ingress controller")

	stopCh := c.StopChannel()
	go c.runWatcher(stopCh)

	<-stopCh
	glog.Infof("shutting down ingress controller")
}

func (c *IngressController) runWatcher(stopCh <-chan struct{}) {
	runOnce := func() (bool, error) {
		var listOpts v1.ListOptions
		glog.Warningf("querying without label filter")
		//listOpts.LabelSelector = labels.Everything()
		glog.Warningf("querying without field filter")
		//listOpts.FieldSelector = fields.Everything()
		ingressList, err := c.kubeClient.Ingresses("").List(listOpts)
		if err != nil {
			return false, fmt.Errorf("error listing ingresss: %v", err)
		}
		for i := range ingressList.Items {
			ingress := &ingressList.Items[i]
			glog.V(4).Infof("found ingress: %v", ingress.Name)
			c.updateIngressRecords(ingress)
		}
		c.scope.MarkReady()

		glog.Warningf("querying without label filter")
		//listOpts.LabelSelector = labels.Everything()
		glog.Warningf("querying without field filter")
		//listOpts.FieldSelector = fields.Everything()
		listOpts.Watch = true
		listOpts.ResourceVersion = ingressList.ResourceVersion
		watcher, err := c.kubeClient.Ingresses("").Watch(listOpts)
		if err != nil {
			return false, fmt.Errorf("error watching ingresss: %v", err)
		}
		ch := watcher.ResultChan()
		for {
			select {
			case <-stopCh:
				glog.Infof("Got stop signal")
				return true, nil
			case event, ok := <-ch:
				if !ok {
					glog.Infof("ingress watch channel closed")
					return false, nil
				}

				ingress := event.Object.(*v1beta1.Ingress)
				glog.V(4).Infof("ingress changed: %s %v", event.Type, ingress.Name)

				switch event.Type {
				case watch.Added, watch.Modified:
					c.updateIngressRecords(ingress)

				case watch.Deleted:
					c.scope.Replace(ingress.Name, nil)

				default:
					glog.Warningf("Unknown event type: %v", event.Type)
				}
			}
		}
	}

	for {
		stop, err := runOnce()
		if stop {
			return
		}

		if err != nil {
			glog.Warningf("Unexpected error in event watch, will retry: %v", err)
			time.Sleep(10 * time.Second)
		}
	}
}

func preferCNAMEs(records []dns.Record) []dns.Record {
	var cnames []dns.Record
	var as []dns.Record

	for _, record := range records {
		if record.RecordType == dns.RecordTypeCNAME {
			cnames = append(cnames, record)
		} else if record.RecordType == dns.RecordTypeA {
			as = append(as, record)
		}
	}

	if len(cnames) > 0 {
		return cnames
	}

	return as
}

func (c *IngressController) updateIngressRecords(ingress *v1beta1.Ingress) {
	var records []dns.Record

	var ingresses []dns.Record
	for i := range ingress.Status.LoadBalancer.Ingress {
		ingress := &ingress.Status.LoadBalancer.Ingress[i]
		if ingress.Hostname != "" {
			// TODO: Support ELB aliases
			ingresses = append(ingresses, dns.Record{
				RecordType: dns.RecordTypeCNAME,
				Value:      ingress.Hostname,
			})
		}
		if ingress.IP != "" {
			ingresses = append(ingresses, dns.Record{
				RecordType: dns.RecordTypeA,
				Value:      ingress.IP,
			})
		}
	}

	for _, rule := range ingress.Spec.Rules {
		if rule.Host == "" {
			continue
		}

		fqdn := dns.EnsureDotSuffix(rule.Host)
		for _, ingress := range ingresses {
			var r dns.Record
			r = ingress
			r.FQDN = fqdn
			records = append(records, r)
		}
	}

	records = preferCNAMEs(records)
	c.scope.Replace(ingress.Name, records)
}
