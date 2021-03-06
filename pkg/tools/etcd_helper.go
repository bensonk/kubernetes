/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package tools

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/conversion"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/tools/metrics"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/coreos/go-etcd/etcd"

	"github.com/golang/glog"
)

func NewEtcdHelper(client EtcdClient, codec runtime.Codec, prefix string) EtcdHelper {
	return EtcdHelper{
		Client:     client,
		Codec:      codec,
		Versioner:  APIObjectVersioner{},
		Copier:     api.Scheme,
		PathPrefix: prefix,
		cache:      util.NewCache(maxEtcdCacheEntries),
	}
}

// EtcdHelper is the reference implementation of StorageInterface.
// TODO(wojtekt): Make it private and expose only StorageInterface to outside world.
type EtcdHelper struct {
	Client EtcdClient
	Codec  runtime.Codec
	Copier runtime.ObjectCopier
	// optional, has to be set to perform any atomic operations
	Versioner EtcdVersioner
	// prefix for all etcd keys
	PathPrefix string

	// We cache objects stored in etcd. For keys we use Node.ModifiedIndex which is equivalent
	// to resourceVersion.
	// This depends on etcd's indexes being globally unique across all objects/types. This will
	// have to revisited if we decide to do things like multiple etcd clusters, or etcd will
	// support multi-object transaction that will result in many objects with the same index.
	// Number of entries stored in the cache is controlled by maxEtcdCacheEntries constant.
	// TODO: Measure how much this cache helps after the conversion code is optimized.
	cache util.Cache
}

func init() {
	metrics.Register()
}

// Implements StorageInterface.
func (h *EtcdHelper) CreateObj(key string, obj, out runtime.Object, ttl uint64) error {
	key = h.prefixEtcdKey(key)
	data, err := h.Codec.Encode(obj)
	if err != nil {
		return err
	}
	if h.Versioner != nil {
		if version, err := h.Versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
			return errors.New("resourceVersion may not be set on objects to be created")
		}
	}

	startTime := time.Now()
	response, err := h.Client.Create(key, string(data), ttl)
	metrics.RecordEtcdRequestLatency("create", getTypeName(obj), startTime)
	if err != nil {
		return err
	}
	if out != nil {
		if _, err := conversion.EnforcePtr(out); err != nil {
			panic("unable to convert output object to pointer")
		}
		_, _, err = h.extractObj(response, err, out, false, false)
	}
	return err
}

// Implements StorageInterface.
func (h *EtcdHelper) SetObj(key string, obj, out runtime.Object, ttl uint64) error {
	var response *etcd.Response
	data, err := h.Codec.Encode(obj)
	if err != nil {
		return err
	}
	key = h.prefixEtcdKey(key)

	create := true
	if h.Versioner != nil {
		if version, err := h.Versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
			create = false
			startTime := time.Now()
			response, err = h.Client.CompareAndSwap(key, string(data), ttl, "", version)
			metrics.RecordEtcdRequestLatency("compareAndSwap", getTypeName(obj), startTime)
			if err != nil {
				return err
			}
		}
	}
	if create {
		// Create will fail if a key already exists.
		startTime := time.Now()
		response, err = h.Client.Create(key, string(data), ttl)
		metrics.RecordEtcdRequestLatency("create", getTypeName(obj), startTime)
	}

	if err != nil {
		return err
	}
	if out != nil {
		if _, err := conversion.EnforcePtr(out); err != nil {
			panic("unable to convert output object to pointer")
		}
		_, _, err = h.extractObj(response, err, out, false, false)
	}

	return err
}

// Implements StorageInterface.
func (h *EtcdHelper) DeleteObj(key string, out runtime.Object) error {
	key = h.prefixEtcdKey(key)
	if _, err := conversion.EnforcePtr(out); err != nil {
		panic("unable to convert output object to pointer")
	}

	startTime := time.Now()
	response, err := h.Client.Delete(key, false)
	metrics.RecordEtcdRequestLatency("delete", getTypeName(out), startTime)
	if !IsEtcdNotFound(err) {
		// if the object that existed prior to the delete is returned by etcd, update out.
		if err != nil || response.PrevNode != nil {
			_, _, err = h.extractObj(response, err, out, false, true)
		}
	}
	return err
}

// Implements StorageInterface.
func (h *EtcdHelper) Delete(key string, recursive bool) error {
	key = h.prefixEtcdKey(key)
	startTime := time.Now()
	_, err := h.Client.Delete(key, recursive)
	metrics.RecordEtcdRequestLatency("delete", "UNKNOWN", startTime)
	return err
}

// Implements StorageInterface.
func (h *EtcdHelper) ExtractObj(key string, objPtr runtime.Object, ignoreNotFound bool) error {
	key = h.prefixEtcdKey(key)
	_, _, _, err := h.bodyAndExtractObj(key, objPtr, ignoreNotFound)
	return err
}

// bodyAndExtractObj performs the normal Get path to etcd, returning the parsed node and response for additional information
// about the response, like the current etcd index and the ttl.
func (h *EtcdHelper) bodyAndExtractObj(key string, objPtr runtime.Object, ignoreNotFound bool) (body string, node *etcd.Node, res *etcd.Response, err error) {
	startTime := time.Now()
	response, err := h.Client.Get(key, false, false)
	metrics.RecordEtcdRequestLatency("get", getTypeName(objPtr), startTime)

	if err != nil && !IsEtcdNotFound(err) {
		return "", nil, nil, err
	}
	body, node, err = h.extractObj(response, err, objPtr, ignoreNotFound, false)
	return body, node, response, err
}

func (h *EtcdHelper) extractObj(response *etcd.Response, inErr error, objPtr runtime.Object, ignoreNotFound, prevNode bool) (body string, node *etcd.Node, err error) {
	if response != nil {
		if prevNode {
			node = response.PrevNode
		} else {
			node = response.Node
		}
	}
	if inErr != nil || node == nil || len(node.Value) == 0 {
		if ignoreNotFound {
			v, err := conversion.EnforcePtr(objPtr)
			if err != nil {
				return "", nil, err
			}
			v.Set(reflect.Zero(v.Type()))
			return "", nil, nil
		} else if inErr != nil {
			return "", nil, inErr
		}
		return "", nil, fmt.Errorf("unable to locate a value on the response: %#v", response)
	}
	body = node.Value
	err = h.Codec.DecodeInto([]byte(body), objPtr)
	if h.Versioner != nil {
		_ = h.Versioner.UpdateObject(objPtr, node.Expiration, node.ModifiedIndex)
		// being unable to set the version does not prevent the object from being extracted
	}
	return body, node, err
}

// Implements StorageInterface.
func (h *EtcdHelper) ExtractObjToList(key string, listObj runtime.Object) error {
	trace := util.NewTrace("ExtractObjToList " + getTypeName(listObj))
	listPtr, err := runtime.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	key = h.prefixEtcdKey(key)
	startTime := time.Now()
	trace.Step("About to read etcd node")
	response, err := h.Client.Get(key, false, false)
	metrics.RecordEtcdRequestLatency("get", getTypeName(listPtr), startTime)
	trace.Step("Etcd node read")
	if err != nil {
		if IsEtcdNotFound(err) {
			return nil
		}
		return err
	}

	nodes := make([]*etcd.Node, 0)
	nodes = append(nodes, response.Node)

	if err := h.decodeNodeList(nodes, listPtr); err != nil {
		return err
	}
	trace.Step("Object decoded")
	if h.Versioner != nil {
		if err := h.Versioner.UpdateList(listObj, response.EtcdIndex); err != nil {
			return err
		}
	}
	return nil
}

// decodeNodeList walks the tree of each node in the list and decodes into the specified object
func (h *EtcdHelper) decodeNodeList(nodes []*etcd.Node, slicePtr interface{}) error {
	trace := util.NewTrace("decodeNodeList " + getTypeName(slicePtr))
	defer trace.LogIfLong(500 * time.Millisecond)
	v, err := conversion.EnforcePtr(slicePtr)
	if err != nil || v.Kind() != reflect.Slice {
		// This should not happen at runtime.
		panic("need ptr to slice")
	}
	for _, node := range nodes {
		if node.Dir {
			trace.Step("Decoding dir " + node.Key + " START")
			if err := h.decodeNodeList(node.Nodes, slicePtr); err != nil {
				return err
			}
			trace.Step("Decoding dir " + node.Key + " END")
			continue
		}
		if obj, found := h.getFromCache(node.ModifiedIndex); found {
			v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
		} else {
			obj := reflect.New(v.Type().Elem())
			if err := h.Codec.DecodeInto([]byte(node.Value), obj.Interface().(runtime.Object)); err != nil {
				return err
			}
			if h.Versioner != nil {
				// being unable to set the version does not prevent the object from being extracted
				_ = h.Versioner.UpdateObject(obj.Interface().(runtime.Object), node.Expiration, node.ModifiedIndex)
			}
			v.Set(reflect.Append(v, obj.Elem()))
			if node.ModifiedIndex != 0 {
				h.addToCache(node.ModifiedIndex, obj.Interface().(runtime.Object))
			}
		}
	}
	trace.Step(fmt.Sprintf("Decoded %v nodes", len(nodes)))
	return nil
}

// Implements StorageInterface.
func (h *EtcdHelper) ExtractToList(key string, listObj runtime.Object) error {
	trace := util.NewTrace("ExtractToList " + getTypeName(listObj))
	defer trace.LogIfLong(time.Second)
	listPtr, err := runtime.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	key = h.prefixEtcdKey(key)
	startTime := time.Now()
	trace.Step("About to list etcd node")
	nodes, index, err := h.listEtcdNode(key)
	metrics.RecordEtcdRequestLatency("list", getTypeName(listPtr), startTime)
	trace.Step("Etcd node listed")
	if err != nil {
		return err
	}
	if err := h.decodeNodeList(nodes, listPtr); err != nil {
		return err
	}
	trace.Step("Node list decoded")
	if h.Versioner != nil {
		if err := h.Versioner.UpdateList(listObj, index); err != nil {
			return err
		}
	}
	return nil
}

func (h *EtcdHelper) listEtcdNode(key string) ([]*etcd.Node, uint64, error) {
	result, err := h.Client.Get(key, true, true)
	if err != nil {
		index, ok := etcdErrorIndex(err)
		if !ok {
			index = 0
		}
		nodes := make([]*etcd.Node, 0)
		if IsEtcdNotFound(err) {
			return nodes, index, nil
		} else {
			return nodes, index, err
		}
	}
	return result.Node.Nodes, result.EtcdIndex, nil
}

type SimpleEtcdUpdateFunc func(runtime.Object) (runtime.Object, error)

// SimpleUpdateFunc converts SimpleEtcdUpdateFunc into EtcdUpdateFunc
func SimpleUpdate(fn SimpleEtcdUpdateFunc) StorageUpdateFunc {
	return func(input runtime.Object, _ ResponseMeta) (runtime.Object, *uint64, error) {
		out, err := fn(input)
		return out, nil, err
	}
}

// Implements StorageInterface.
func (h *EtcdHelper) GuaranteedUpdate(key string, ptrToType runtime.Object, ignoreNotFound bool, tryUpdate StorageUpdateFunc) error {
	v, err := conversion.EnforcePtr(ptrToType)
	if err != nil {
		// Panic is appropriate, because this is a programming error.
		panic("need ptr to type")
	}
	key = h.prefixEtcdKey(key)
	for {
		obj := reflect.New(v.Type()).Interface().(runtime.Object)
		origBody, node, res, err := h.bodyAndExtractObj(key, obj, ignoreNotFound)
		if err != nil {
			return err
		}
		meta := ResponseMeta{}
		if node != nil {
			meta.TTL = node.TTL
			if node.Expiration != nil {
				meta.Expiration = node.Expiration
			}
			meta.ResourceVersion = node.ModifiedIndex
		}
		// Get the object to be written by calling tryUpdate.
		ret, newTTL, err := tryUpdate(obj, meta)
		if err != nil {
			return err
		}

		index := uint64(0)
		ttl := uint64(0)
		if node != nil {
			index = node.ModifiedIndex
			if node.TTL > 0 {
				ttl = uint64(node.TTL)
			}
		} else if res != nil {
			index = res.EtcdIndex
		}

		if newTTL != nil {
			ttl = *newTTL
		}

		data, err := h.Codec.Encode(ret)
		if err != nil {
			return err
		}

		// First time this key has been used, try creating new value.
		if index == 0 {
			startTime := time.Now()
			response, err := h.Client.Create(key, string(data), ttl)
			metrics.RecordEtcdRequestLatency("create", getTypeName(ptrToType), startTime)
			if IsEtcdNodeExist(err) {
				continue
			}
			_, _, err = h.extractObj(response, err, ptrToType, false, false)
			return err
		}

		if string(data) == origBody {
			return nil
		}

		startTime := time.Now()
		// Swap origBody with data, if origBody is the latest etcd data.
		response, err := h.Client.CompareAndSwap(key, string(data), ttl, origBody, index)
		metrics.RecordEtcdRequestLatency("compareAndSwap", getTypeName(ptrToType), startTime)
		if IsEtcdTestFailed(err) {
			// Try again.
			continue
		}
		_, _, err = h.extractObj(response, err, ptrToType, false, false)
		return err
	}
}

func (h *EtcdHelper) prefixEtcdKey(key string) string {
	if strings.HasPrefix(key, path.Join("/", h.PathPrefix)) {
		return key
	}
	return path.Join("/", h.PathPrefix, key)
}

// etcdCache defines interface used for caching objects stored in etcd. Objects are keyed by
// their Node.ModifiedIndex, which is unique across all types.
// All implementations must be thread-safe.
type etcdCache interface {
	getFromCache(index uint64) (runtime.Object, bool)
	addToCache(index uint64, obj runtime.Object)
}

const maxEtcdCacheEntries int = 50000

func getTypeName(obj interface{}) string {
	return reflect.TypeOf(obj).String()
}

func (h *EtcdHelper) getFromCache(index uint64) (runtime.Object, bool) {
	startTime := time.Now()
	defer func() {
		metrics.ObserveGetCache(startTime)
	}()
	obj, found := h.cache.Get(index)
	if found {
		// We should not return the object itself to avoid poluting the cache if someone
		// modifies returned values.
		objCopy, err := h.Copier.Copy(obj.(runtime.Object))
		if err != nil {
			glog.Errorf("Error during DeepCopy of cached object: %q", err)
			return nil, false
		}
		metrics.ObserveCacheHit()
		return objCopy.(runtime.Object), true
	}
	metrics.ObserveCacheMiss()
	return nil, false
}

func (h *EtcdHelper) addToCache(index uint64, obj runtime.Object) {
	startTime := time.Now()
	defer func() {
		metrics.ObserveAddCache(startTime)
	}()
	objCopy, err := h.Copier.Copy(obj)
	if err != nil {
		glog.Errorf("Error during DeepCopy of cached object: %q", err)
		return
	}
	isOverwrite := h.cache.Add(index, objCopy)
	if !isOverwrite {
		metrics.ObserveNewEntry()
	}
}
