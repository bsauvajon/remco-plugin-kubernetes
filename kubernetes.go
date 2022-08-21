package main

import (
	"encoding/json"
	"net/rpc/jsonrpc"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/natefinch/pie"

	"github.com/HeavyHorst/remco/pkg/backends/plugin"
	log "github.com/HeavyHorst/remco/pkg/log"

	"k8s.io/api/apps/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	// Resync period for the kube controller loop.
	resyncPeriod = 60 * time.Second
)

func main() {
	p := pie.NewProvider()
	log.Debug("lancement du plugin")
	if err := p.RegisterName("Plugin", &KubernetesRPCServer{}); err != nil {
		log.Fatal("failed to register Plugin: %s", err)
	}
	p.ServeCodec(jsonrpc.NewServerCodec)
}

type KubernetesRPCServer struct {
	kubeClient *kubernetes.Clientset

	types map[string]*listener

	// First Key is the typ
	// Second key is the namespace
	// Third key is the name
	// Value is the jsonified object (Either Service, Endpoints or Statefulset)
	data map[string]map[string]map[string]string

	lock      sync.Mutex
	newChange chan struct{}
	// Already registered listeners
	listeners map[string]struct{}
}

type listener struct {
	typ        runtime.Object
	restClient rest.Interface
}

func (e *KubernetesRPCServer) Init(args map[string]interface{}, resp *bool) error {

	var err error
	config := &rest.Config{}

	if loglevel, ok := args["log_level"].(string); ok {
		log.InitializeLogging("test", loglevel)
	}

	if host, ok := args["host"].(string); ok {
		log.Debug("Connection from outside the kubernetes cluster", "Host", host)

		config.Host = host

		if username, ok := args["username"].(string); ok {
			config.Username = username
		}
		if password, ok := args["password"].(string); ok {
			config.Password = password
		}
		if token, ok := args["token"].(string); ok {
			config.BearerToken = token
		}

		config.TLSClientConfig = rest.TLSClientConfig{}

		if cert, ok := args["cert"].(string); ok {
			config.TLSClientConfig.CertFile = cert
		}
		if key, ok := args["key"].(string); ok {
			config.TLSClientConfig.KeyFile = key
		}
		if ca, ok := args["ca"].(string); ok {
			config.TLSClientConfig.CAFile = ca
		}
	} else {
		log.Debug("Connection from inside a kubernetes cluster")

		config, err = rest.InClusterConfig()
		if err != nil {
			return err
		}
	}

	// Use protobuf for communication with apiserver.
	config.ContentType = "application/vnd.kubernetes.protobuf"

	// creates the clientset
	e.kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Error("Error connecting to kubernetes", "cause", err)
		return err
	}

	// pods, err := client.kubeClient.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	// if err != nil {
	// 	panic(err.Error())
	// }
	// log.Info("pods in the cluster", "count", len(pods.Items))

	e.data = map[string]map[string]map[string]string{}
	e.listeners = map[string]struct{}{}
	e.newChange = make(chan struct{}, 1)
	e.types = map[string]*listener{
		"services": &listener{
			typ:        &v1.Service{},
			restClient: e.kubeClient.CoreV1().RESTClient(),
		},
		"endpoints": &listener{
			typ:        &v1.Endpoints{},
			restClient: e.kubeClient.CoreV1().RESTClient(),
		},
		"statefulsets": &listener{
			typ:        &v1beta1.StatefulSet{},
			restClient: e.kubeClient.AppsV1beta1().RESTClient(),
		},
	}

	return err
}

func (e *KubernetesRPCServer) GetValues(args []string, resp *map[string]string) error {

	log.Debug("GetValues called: ", "args", hclog.Fmt("%v", args))

	e.lock.Lock()
	defer e.lock.Unlock()
	result := map[string]string{}

	for _, key := range args {
		// TODO: Refacto this ugly inner loop
		log.Debug("KEY", "key", hclog.Fmt("%s", key))

		result[key] = ""

		if key == "/" {
			// Returns eveything
			for typ, typMap := range e.data {
				for ns, nsMap := range typMap {
					for el, elData := range nsMap {
						result[path.Join(typ, ns, el)] = elData
					}
				}
			}
		}

		parts := strings.Split(strings.Trim(key, "/"), "/")

		if len(parts) == 1 {
			// Return all the entries for a type
			typ := parts[0]
			typMap, ok := e.data[typ]
			if !ok {
				continue
			}
			for ns, nsMap := range typMap {
				for el, elData := range nsMap {
					result[path.Join(typ, ns, el)] = elData
				}
			}
		} else if len(parts) == 2 {
			// Return all the entries for a type + namespace
			typ := parts[0]
			typMap, ok := e.data[typ]
			if !ok {
				continue
			}
			ns := parts[1]
			nsMap, ok := typMap[ns]
			if !ok {
				continue
			}
			for el, elData := range nsMap {
				result[path.Join(typ, ns, el)] = elData
			}
		} else if len(parts) == 3 {
			// Return all the entries for a type + namespace + name
			typ := parts[0]
			typMap, ok := e.data[typ]
			if !ok {
				continue
			}
			ns := parts[1]
			nsMap, ok := typMap[ns]
			if !ok {
				continue
			}
			el := parts[2]
			elData, ok := nsMap[el]
			if ok {
				result[path.Join(typ, ns, el)] = elData
			}
		} else {
			log.Debug("Invalid key: %s", key)
		}
	}
	*resp = result
	return nil
}

func (e *KubernetesRPCServer) Close(args interface{}, resp *interface{}) error {
	e.lock.Unlock()
	return nil
}

func (e *KubernetesRPCServer) WatchPrefix(args plugin.WatchConfig, resp *uint64) error {

	log.Debug("Watch called: ", "prefix", args.Prefix, "keys", args.Opts.Keys, "waitIndex", args.Opts.WaitIndex)

	e.lock.Lock()
	for _, key := range args.Opts.Keys {
		p := path.Join(args.Prefix, key)
		if _, ok := e.listeners[p]; ok {
			// Listener already installed
			continue
		}

		s := strings.Split(strings.Trim(p, "/"), "/")

		if len(s) != 1 && len(s) != 2 {
			log.Debug("Invalid watch", "required", p, "Only /<type>/{namespace} allowed")
			continue
		}
		t, ok := e.types[s[0]]
		if !ok {
			log.Debug("Unknown type", "type", s[0])
			continue
		}
		namespace := v1.NamespaceAll
		if len(s) == 2 {
			namespace = s[1]
		}
		log.Debug("Creating controller", "type", s[0])
		_, controller := cache.NewInformer(
			cache.NewListWatchFromClient(
				t.restClient,
				s[0],
				namespace,
				fields.Everything()),
			t.typ,
			resyncPeriod,
			cache.ResourceEventHandlerFuncs{
				AddFunc:    e.add(s[0]),
				DeleteFunc: e.remove(s[0]),
				UpdateFunc: e.update(s[0]),
			},
		)

		log.Debug("Starting controller", "type", s[0])
		// TODO: Make the exit work
		waitChan := make(chan struct{})
		go controller.Run(waitChan)
		e.listeners[p] = struct{}{}
	}

	e.lock.Unlock()

	// Wait for a new change
	log.Debug("Waiting for change")
	<-e.newChange

	log.Debug("Triggering an update")
	*resp = args.Opts.WaitIndex + 1
	return nil
}

func (e *KubernetesRPCServer) add(typ string) func(obj interface{}) {
	return func(obj interface{}) {
		log.Debug("add called")
		e.addObject(typ, obj)
	}
}

func (e *KubernetesRPCServer) update(typ string) func(oldObj, newObj interface{}) {
	return func(oldObj, newObj interface{}) {
		log.Debug("update called %#v %#v", oldObj, newObj)
		e.deleteObject(typ, oldObj)
		e.addObject(typ, newObj)
	}
}

func (e *KubernetesRPCServer) remove(typ string) func(obj interface{}) {
	return func(obj interface{}) {
		log.Debug("remove called")
		e.deleteObject(typ, obj)
	}
}

func (e *KubernetesRPCServer) addObject(typ string, obj interface{}) {
	meta, ok := getMeta(obj)
	if !ok {
		return
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	t, ok := e.data[typ]
	if !ok {
		t = map[string]map[string]string{}
		e.data[typ] = t
	}
	n, ok := t[meta.Namespace]
	if !ok {
		n = map[string]string{}
		t[meta.Namespace] = n
	}
	js, err := json.Marshal(obj)
	if err != nil {
		log.Debug("Unable to marshal object: %s", err)
		return
	}
	n[meta.Name] = string(js)
	select {
	case e.newChange <- struct{}{}:
		log.Debug("Change posted")
	default:
		log.Debug("Change NOT posted")
	}
}
func (e *KubernetesRPCServer) deleteObject(typ string, obj interface{}) {
	meta, ok := getMeta(obj)
	if !ok {
		return
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	t, ok := e.data[typ]
	if !ok {
		log.Debug("Delete but no old data for type %s", typ)
		return
	}
	n, ok := t[meta.Namespace]
	if !ok {
		log.Debug("Delete but no old data for type/namespace %s/%s", typ, meta.Namespace)
		return
	}
	delete(n, meta.Name)
	select {
	case e.newChange <- struct{}{}:
		log.Debug("Change posted")
	default:
		log.Debug("Change NOT posted")
	}
}

func getMeta(obj interface{}) (*metav1.ObjectMeta, bool) {
	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr {
		return nil, false
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	f := v.FieldByName("ObjectMeta")
	if f == reflect.Zero(f.Type()) {
		return nil, false
	}
	meta, ok := f.Interface().(metav1.ObjectMeta)
	if ok {
		if meta.Namespace == "" {
			log.Debug("Namespace empty for object", "object", obj)
		}
		if meta.Name == "" {
			log.Debug("Name empty for object", "object", obj)
		}
	}
	return &meta, ok
}
