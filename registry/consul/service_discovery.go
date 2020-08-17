/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package consul

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

import (
	"github.com/dubbogo/gost/container/set"
	"github.com/dubbogo/gost/page"
	consul "github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	perrors "github.com/pkg/errors"
)

import (
	"github.com/apache/dubbo-go/common"
	"github.com/apache/dubbo-go/common/constant"
	"github.com/apache/dubbo-go/common/extension"
	"github.com/apache/dubbo-go/common/logger"
	"github.com/apache/dubbo-go/config"
	"github.com/apache/dubbo-go/registry"
)

const (
	enable = "enable"
)

var (
	// 16 would be enough. We won't use concurrentMap because in most cases, there are not race condition
	instanceMap = make(map[string]registry.ServiceDiscovery, 16)
	initLock    sync.Mutex
)

// init will put the service discovery into extension
func init() {
	extension.SetServiceDiscovery(constant.CONSUL_KEY, newConsulServiceDiscovery)
}

// newConsulServiceDiscovery will create new service discovery instance
// use double-check pattern to reduce race condition
func newConsulServiceDiscovery(name string) (registry.ServiceDiscovery, error) {
	instance, ok := instanceMap[name]
	if ok {
		return instance, nil
	}

	initLock.Lock()
	defer initLock.Unlock()

	// double check
	instance, ok = instanceMap[name]
	if ok {
		return instance, nil
	}

	sdc, ok := config.GetBaseConfig().GetServiceDiscoveries(name)
	if !ok || len(sdc.RemoteRef) == 0 {
		return nil, perrors.New("could not init the instance because the config is invalid")
	}

	remoteConfig, ok := config.GetBaseConfig().GetRemoteConfig(sdc.RemoteRef)
	if !ok {
		return nil, perrors.New("could not find the remote config for name: " + sdc.RemoteRef)
	}

	descriptor := fmt.Sprintf("consul-service-discovery[%s]", remoteConfig.Address)

	return &consulServiceDiscovery{
		address:    remoteConfig.Address,
		descriptor: descriptor,
		ttl:        make(map[string]chan struct{}),
	}, nil
}

// nacosServiceDiscovery is the implementation of service discovery based on nacos.
// There is a problem, the go client for nacos does not support the id field.
// we will use the metadata to store the id of ServiceInstance
type consulServiceDiscovery struct {
	group string
	// descriptor is a short string about the basic information of this instance
	descriptor string
	// Consul client.
	consulClient      *consul.Client
	serviceUrl        common.URL
	checkPassInterval int64
	tag               string
	tags              []string
	address           string
	ttl               map[string]chan struct{}
	*consul.Config
}

func (csd *consulServiceDiscovery) Init(registryURL common.URL) error {
	csd.serviceUrl = registryURL
	csd.checkPassInterval = registryURL.GetParamInt(constant.CHECK_PASS_INTERVAL, constant.DEFAULT_CHECK_PASS_INTERVAL)
	csd.tag = registryURL.GetParam(constant.QUERY_TAG, "")
	csd.tags = strings.Split(registryURL.GetParam("tags", ""), ",")
	aclToken := registryURL.GetParam(constant.ACL_TOKEN, "")
	csd.Config = &consul.Config{Address: csd.address, Token: aclToken}
	client, err := consul.NewClient(csd.Config)
	if err != nil {
		return perrors.WithMessage(err, "create consul client failed.")
	}
	csd.consulClient = client
	return nil
}

func (csd *consulServiceDiscovery) String() string {
	return csd.descriptor
}

func (csd *consulServiceDiscovery) Destroy() error {
	csd.consulClient = nil
	for _, t := range csd.ttl {
		close(t)
	}
	csd.ttl = nil
	return nil
}

func (csd *consulServiceDiscovery) Register(instance registry.ServiceInstance) error {
	ins, _ := csd.buildRegisterInstance(instance)
	err := csd.consulClient.Agent().ServiceRegister(ins)
	if err != nil {
		return perrors.WithMessage(err, "consul could not register the instance. "+instance.GetServiceName())
	}

	return csd.registerTtl(instance)
}

func (csd *consulServiceDiscovery) registerTtl(instance registry.ServiceInstance) error {
	checkID := buildID(instance)

	stopChan := make(chan struct{})
	csd.ttl[buildID(instance)] = stopChan

	period := time.Duration(csd.checkPassInterval/8) * time.Millisecond
	timer := time.NewTimer(period)
	go func() {
		for {
			select {
			case <-timer.C:
				timer.Reset(period)
				err := csd.consulClient.Agent().PassTTL(checkID, "")
				if err != nil {
					logger.Warnf("pass ttl heartbeat fail:%v", err)
					break
				}
				logger.Debugf("passed ttl heartbeat for %s", checkID)
				break
			case <-stopChan:
				logger.Info("ttl %s for service %s is stopped", checkID, instance.GetServiceName())
				return
			}
		}
	}()
	return nil
}

func (csd *consulServiceDiscovery) Update(instance registry.ServiceInstance) error {
	ins, _ := csd.buildRegisterInstance(instance)
	err := csd.consulClient.Agent().ServiceDeregister(buildID(instance))
	if err != nil {
		logger.Warnf("unregister instance %s fail:%v", instance.GetServiceName(), err)
	}
	return csd.consulClient.Agent().ServiceRegister(ins)
}

func (csd *consulServiceDiscovery) Unregister(instance registry.ServiceInstance) error {
	err := csd.consulClient.Agent().ServiceDeregister(buildID(instance))
	if err != nil {
		logger.Errorf("unregister service instance %s,error: %v", instance.GetId(), err)
		return err
	}
	stopChanel, ok := csd.ttl[buildID(instance)]
	if !ok {
		logger.Warnf("ttl for service instance %s didn't exist", instance.GetId())
	} else {
		close(stopChanel)
		delete(csd.ttl, buildID(instance))
	}
	return nil
}

func (csd *consulServiceDiscovery) GetDefaultPageSize() int {
	return registry.DefaultPageSize
}

func (csd *consulServiceDiscovery) GetServices() *gxset.HashSet {

	var res = gxset.NewSet()
	services, _, err := csd.consulClient.Catalog().Services(nil)
	if err != nil {
		logger.Errorf("get services,error: %v", err)
		return res
	}

	for service, _ := range services {
		res.Add(service)
	}
	return res

}

func (csd *consulServiceDiscovery) GetInstances(serviceName string) []registry.ServiceInstance {
	waitTime := csd.serviceUrl.GetParamInt(constant.WATCH_TIMEOUT, constant.DEFAULT_WATCH_TIMEOUT) / 1000
	instances, _, err := csd.consulClient.Health().Service(serviceName, csd.tag, true, &consul.QueryOptions{
		WaitTime: time.Duration(waitTime),
	})
	if err != nil {
		logger.Errorf("get instances for service %s,error: %v", serviceName, err)
		return nil
	}

	res := make([]registry.ServiceInstance, 0, len(instances))
	for _, ins := range instances {
		metadata := ins.Service.Meta

		// enable status
		enableStr := metadata[enable]
		delete(metadata, enable)
		enable, _ := strconv.ParseBool(enableStr)

		// health status
		status := ins.Checks.AggregatedStatus()
		healthy := false
		if status == consul.HealthPassing {
			healthy = true
		}
		res = append(res, &registry.DefaultServiceInstance{
			Id:          ins.Service.ID,
			ServiceName: ins.Service.Service,
			Host:        ins.Service.Address,
			Port:        ins.Service.Port,
			Enable:      enable,
			Healthy:     healthy,
			Metadata:    metadata,
		})
	}

	return res
}

func (csd *consulServiceDiscovery) GetInstancesByPage(serviceName string, offset int, pageSize int) gxpage.Pager {
	all := csd.GetInstances(serviceName)
	res := make([]interface{}, 0, pageSize)
	for i := offset; i < len(all) && i < offset+pageSize; i++ {
		res = append(res, all[i])
	}
	return gxpage.New(offset, pageSize, res, len(all))
}

func (csd *consulServiceDiscovery) GetHealthyInstancesByPage(serviceName string, offset int, pageSize int, healthy bool) gxpage.Pager {
	all := csd.GetInstances(serviceName)
	res := make([]interface{}, 0, pageSize)
	// could not use res = all[a:b] here because the res should be []interface{}, not []ServiceInstance
	var (
		i     = offset
		count = 0
	)
	for i < len(all) && count < pageSize {
		ins := all[i]
		if ins.IsHealthy() == healthy {
			res = append(res, all[i])
			count++
		}
		i++
	}
	return gxpage.New(offset, pageSize, res, len(all))
}

func (csd *consulServiceDiscovery) GetRequestInstances(serviceNames []string, offset int, requestedSize int) map[string]gxpage.Pager {
	res := make(map[string]gxpage.Pager, len(serviceNames))
	for _, name := range serviceNames {
		res[name] = csd.GetInstancesByPage(name, offset, requestedSize)
	}
	return res
}

func (csd *consulServiceDiscovery) AddListener(listener *registry.ServiceInstancesChangedListener) error {

	params := make(map[string]interface{}, 8)
	params["type"] = "service"
	params["service"] = listener.ServiceName
	params["passingonly"] = true
	plan, err := watch.Parse(params)
	if err != nil {
		logger.Errorf("add listener for service %s,error:%v", listener.ServiceName, err)
		return err
	}

	plan.Handler = func(idx uint64, raw interface{}) {
		services, ok := raw.([]*consul.ServiceEntry)
		if !ok {
			err = perrors.New("handler get non ServiceEntry type parameter")
			return
		}
		instances := make([]registry.ServiceInstance, 0, len(services))
		for _, ins := range services {
			metadata := ins.Service.Meta

			// enable status
			enableStr := metadata[enable]
			delete(metadata, enable)
			enable, _ := strconv.ParseBool(enableStr)

			// health status
			status := ins.Checks.AggregatedStatus()
			healthy := false
			if status == consul.HealthPassing {
				healthy = true
			}
			instances = append(instances, &registry.DefaultServiceInstance{
				Id:          ins.Service.ID,
				ServiceName: ins.Service.Service,
				Host:        ins.Service.Address,
				Port:        ins.Service.Port,
				Enable:      enable,
				Healthy:     healthy,
				Metadata:    metadata,
			})
		}
		e := csd.DispatchEventForInstances(listener.ServiceName, instances)
		if e != nil {
			logger.Errorf("Dispatching event got exception, service name: %s, err: %v", listener.ServiceName, err)
		}
	}
	go func() {
		err = plan.RunWithConfig(csd.Config.Address, csd.Config)
		if err != nil {
			logger.Error("consul plan run failure!error:%v", err)
		}
	}()
	return nil
}

func (csd *consulServiceDiscovery) DispatchEventByServiceName(serviceName string) error {
	return csd.DispatchEventForInstances(serviceName, csd.GetInstances(serviceName))
}

func (csd *consulServiceDiscovery) DispatchEventForInstances(serviceName string, instances []registry.ServiceInstance) error {
	return csd.DispatchEvent(registry.NewServiceInstancesChangedEvent(serviceName, instances))
}

func (csd *consulServiceDiscovery) DispatchEvent(event *registry.ServiceInstancesChangedEvent) error {
	extension.GetGlobalDispatcher().Dispatch(event)
	return nil
}

func (csd *consulServiceDiscovery) buildRegisterInstance(instance registry.ServiceInstance) (*consul.AgentServiceRegistration, error) {
	metadata := instance.GetMetadata()
	if metadata == nil {
		metadata = make(map[string]string, 1)
	}
	metadata[enable] = strconv.FormatBool(instance.IsEnable())

	// check
	check := csd.buildCheck(instance)

	return &consul.AgentServiceRegistration{
		ID:      buildID(instance),
		Name:    instance.GetServiceName(),
		Port:    instance.GetPort(),
		Address: instance.GetHost(),
		Meta:    metadata,
		Check:   &check,
	}, nil
}

func (csd *consulServiceDiscovery) buildCheck(instance registry.ServiceInstance) consul.AgentServiceCheck {

	deregister, ok := instance.GetMetadata()[constant.DEREGISTER_AFTER]
	if !ok || deregister == "" {
		deregister = constant.DEFAULT_DEREGISTER_TIME
	}
	return consul.AgentServiceCheck{
		CheckID:                        buildID(instance),
		TTL:                            strconv.FormatInt(csd.checkPassInterval/1000, 10) + "s",
		DeregisterCriticalServiceAfter: deregister,
	}
}

func buildID(instance registry.ServiceInstance) string {

	id := fmt.Sprintf("id:%s,serviceName:%s,host:%s,port:%d", instance.GetId(), instance.GetServiceName(), instance.GetHost(), instance.GetPort())
	return id
}
