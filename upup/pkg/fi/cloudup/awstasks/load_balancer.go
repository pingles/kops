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

package awstasks

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/golang/glog"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"strconv"
)

//go:generate fitask -type=LoadBalancer
type LoadBalancer struct {
	Name *string

	// ID is the name in ELB, possibly different from our name
	// (ELB is restricted as to names, so we have limited choices!)
	ID *string

	DNSName      *string
	HostedZoneId *string

	Subnets        []*Subnet
	SecurityGroups []*SecurityGroup
	// HealthChecks   []*LoadBalancerHealthChecks

	Listeners map[string]*LoadBalancerListener
}

var _ fi.CompareWithID = &LoadBalancer{}

func (e *LoadBalancer) CompareWithID() *string {
	return e.ID
}

type LoadBalancerListener struct {
	InstancePort int
}

func (e *LoadBalancerListener) mapToAWS(loadBalancerPort int64) *elb.Listener {
	return &elb.Listener{
		LoadBalancerPort: aws.Int64(loadBalancerPort),

		Protocol: aws.String("TCP"),

		InstanceProtocol: aws.String("TCP"),
		InstancePort:     aws.Int64(int64(e.InstancePort)),
	}
}

var _ fi.HasDependencies = &LoadBalancerListener{}

func (e *LoadBalancerListener) GetDependencies(tasks map[string]fi.Task) []fi.Task {
	return nil
}

func findELB(cloud awsup.AWSCloud, name string) (*elb.LoadBalancerDescription, error) {
	request := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{&name},
	}

	var found []*elb.LoadBalancerDescription
	err := cloud.ELB().DescribeLoadBalancersPages(request, func(p *elb.DescribeLoadBalancersOutput, lastPage bool) (shouldContinue bool) {
		for _, lb := range p.LoadBalancerDescriptions {
			if aws.StringValue(lb.LoadBalancerName) == name {
				found = append(found, lb)
			} else {
				glog.Warningf("Got ELB with unexpected name")
			}
		}

		return true
	})

	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "LoadBalancerNotFound" {
				return nil, nil
			}
		}

		return nil, fmt.Errorf("error listing ELBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple ELBs with name %q", name)
	}

	return found[0], nil
}

func (e *LoadBalancer) Find(c *fi.Context) (*LoadBalancer, error) {
	cloud := c.Cloud.(awsup.AWSCloud)

	elbName := fi.StringValue(e.ID)
	if elbName == "" {
		elbName = fi.StringValue(e.Name)
	}

	lb, err := findELB(cloud, elbName)
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	actual := &LoadBalancer{}
	actual.Name = e.Name
	actual.ID = lb.LoadBalancerName
	actual.DNSName = lb.DNSName
	actual.HostedZoneId = lb.CanonicalHostedZoneNameID
	for _, subnet := range lb.Subnets {
		actual.Subnets = append(actual.Subnets, &Subnet{ID: subnet})
	}

	for _, sg := range lb.SecurityGroups {
		actual.SecurityGroups = append(actual.SecurityGroups, &SecurityGroup{ID: sg})
	}

	actual.Listeners = make(map[string]*LoadBalancerListener)

	for _, ld := range lb.ListenerDescriptions {
		l := ld.Listener
		loadBalancerPort := strconv.FormatInt(aws.Int64Value(l.LoadBalancerPort), 10)

		actualListener := &LoadBalancerListener{}
		actualListener.InstancePort = int(aws.Int64Value(l.InstancePort))
		actual.Listeners[loadBalancerPort] = actualListener
	}

	// Avoid spurious mismatches
	if subnetSlicesEqualIgnoreOrder(actual.Subnets, e.Subnets) {
		actual.Subnets = e.Subnets
	}
	if e.DNSName == nil {
		e.DNSName = actual.DNSName
	}
	if e.HostedZoneId == nil {
		e.HostedZoneId = actual.HostedZoneId
	}
	if e.ID == nil {
		e.ID = actual.ID
	}

	return actual, nil
}

func (e *LoadBalancer) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (s *LoadBalancer) CheckChanges(a, e, changes *LoadBalancer) error {
	if a == nil {
		if fi.StringValue(e.Name) == "" {
			return fi.RequiredField("Name")
		}
		if len(e.SecurityGroups) == 0 {
			return fi.RequiredField("SecurityGroups")
		}
		if len(e.Subnets) == 0 {
			return fi.RequiredField("Subnets")
		}
	}
	return nil
}

type terraformELB struct {
	Name           string                  `json:"name,omitempty"`
	Subnets        []*terraform.Literal    `json:"subnets,omitempty"`
	SecurityGroups []*terraform.Literal    `json:"security_groups,omitempty"`
	Listeners      []*terraformELBListener `json:"listener,omitempty"`
}

type terraformELBListener struct {
	InstancePort     int64  `json:"instance_port,omitempty"`
	InstanceProtocol string `json:"instance_protocol,omitempty"`
	LBPort           int64  `json:"lb_port,omitempty"`
	LBProtocol       string `json:"lb_protocol,omitempty"`
	SSLCertificateID string `json:"ssl_certificate_id,omitempty"`
}

type terraformELBHealthCheck struct {
	HealthyThreshold   int64  `json:"healthy_threshold,omitempty"`
	UnhealthyThreshold int64  `json:"unhealthy_threshold,omityempty"`
	Target             string `json:"target,omitempty"`
	Interval           int64  `json:"interval,omitempty"`
	Timeout            int64  `json:"timeout,omitempty"`
}

func (_ *LoadBalancer) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *LoadBalancer) error {
	glog.V(2).Infof("Creating Elastic LoadBalancer for VPC")

	tf := &terraformELB{
		Name: *e.Name,
	}
	tf.Subnets = make([]*terraform.Literal, len(e.Subnets))
	for idx, subnet := range e.Subnets {
		tf.Subnets[idx] = subnet.TerraformLink()
	}
	tf.SecurityGroups = make([]*terraform.Literal, len(e.SecurityGroups))
	for idx, group := range e.SecurityGroups {
		tf.SecurityGroups[idx] = group.TerraformLink()
	}
	tf.Listeners = []*terraformELBListener{}

	for loadBalancerPort, listener := range e.Listeners {
		loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
		}
		l := listener.mapToAWS(loadBalancerPortInt)
		listener := &terraformELBListener{
			LBPort:           loadBalancerPortInt,
			LBProtocol:       *l.Protocol,
			InstancePort:     *l.InstancePort,
			InstanceProtocol: *l.InstanceProtocol,
		}
		tf.Listeners = append(tf.Listeners, listener)
	}

	return t.RenderResource("aws_elb", *e.Name, tf)
}

func (e *LoadBalancer) TerraformLink() *terraform.Literal {
	return terraform.LiteralProperty("aws_elb", *e.Name, "id")
}

func (_ *LoadBalancer) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *LoadBalancer) error {
	elbName := e.ID
	if elbName == nil {
		elbName = e.Name
	}

	if elbName == nil {
		return fi.RequiredField("ID")
	}

	if a == nil {
		request := &elb.CreateLoadBalancerInput{}
		request.LoadBalancerName = elbName

		for _, subnet := range e.Subnets {
			request.Subnets = append(request.Subnets, subnet.ID)
		}

		for _, sg := range e.SecurityGroups {
			request.SecurityGroups = append(request.SecurityGroups, sg.ID)
		}

		request.Listeners = []*elb.Listener{}

		for loadBalancerPort, listener := range e.Listeners {
			loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 64)
			if err != nil {
				return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
			}
			awsListener := listener.mapToAWS(loadBalancerPortInt)
			request.Listeners = append(request.Listeners, awsListener)
		}

		glog.V(2).Infof("Creating ELB with Name:%q", *e.ID)

		response, err := t.Cloud.ELB().CreateLoadBalancer(request)
		if err != nil {
			return fmt.Errorf("error creating ELB: %v", err)
		}

		e.DNSName = response.DNSName
		e.ID = elbName

		lb, err := findELB(t.Cloud, *e.ID)
		if err != nil {
			return err
		}
		if lb == nil {
			// TODO: Retry?  Is this async
			return fmt.Errorf("Unable to find newly created ELB")
		}

		e.HostedZoneId = lb.CanonicalHostedZoneNameID
	} else {
		if changes.Subnets != nil {
			return fmt.Errorf("subnet changes on LoadBalancer not yet implemented")
		}

		if changes.Listeners != nil {
			request := &elb.CreateLoadBalancerListenersInput{}
			request.LoadBalancerName = elbName

			for loadBalancerPort, listener := range changes.Listeners {
				loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 64)
				if err != nil {
					return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
				}
				awsListener := listener.mapToAWS(loadBalancerPortInt)
				request.Listeners = append(request.Listeners, awsListener)
			}

			glog.V(2).Infof("Creating LoadBalancer listeners")

			_, err := t.Cloud.ELB().CreateLoadBalancerListeners(request)
			if err != nil {
				return fmt.Errorf("error creating LoadBalancerListeners: %v", err)
			}
		}
	}

	return t.AddELBTags(*e.ID, t.Cloud.BuildTags(e.Name))
}
