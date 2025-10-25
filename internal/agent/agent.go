package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

    "github.com/hackohio/agent/internal/driver"
    tpl "github.com/hackohio/agent/internal/template"
)

type GVR struct {
	Group    string
	Version  string
	Resource string
}

func (g GVR) ToSchema() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g.Group, Version: g.Version, Resource: g.Resource}
}

type Config struct {
    NodeName                 string
    Concurrency              int
    NodeActionGVR            GVR
    InstructionSetGVR        GVR
    DriverAddr               string
    DriverInsecure           bool
}

type Agent struct {
	cfg        Config
	dyn        dynamic.Interface
	clientset  *kubernetes.Clientset
	recorder   record.EventRecorder
	naRes      dynamic.NamespaceableResourceInterface
	isRes      dynamic.NamespaceableResourceInterface
	semaphore  chan struct{}
	grpcDriver *driver.GRPCDriver
}

func New(ctx context.Context, cfg Config) (*Agent, error) {
	rcfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("build in-cluster config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rcfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rcfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "kuberisc-agent"})

	ag := &Agent{
		cfg:       cfg,
		dyn:       dyn,
		clientset: cs,
		recorder:  recorder,
		naRes:     dyn.Resource(cfg.NodeActionGVR.ToSchema()),
		isRes:     dyn.Resource(cfg.InstructionSetGVR.ToSchema()),
		semaphore: make(chan struct{}, cfg.Concurrency),
	}
	if cfg.DriverAddr != "" {
		gd, err := driver.NewGRPCDriver(ctx, driver.GRPCConfig{Addr: cfg.DriverAddr, Insecure: cfg.DriverInsecure})
		if err != nil {
			return nil, fmt.Errorf("connect driver: %w", err)
		}
		ag.grpcDriver = gd
	}
	return ag, nil
}

func (a *Agent) Run(ctx context.Context) error {
	// Initial list
	fieldSel := fields.OneTermEqualSelector("spec.nodeName", a.cfg.NodeName).String()
	listOpts := metav1.ListOptions{FieldSelector: fieldSel}
	list, err := a.naRes.Namespace(metav1.NamespaceAll).List(ctx, listOpts)
	if err != nil {
		return fmt.Errorf("list NodeActions: %w", err)
	}
	for i := range list.Items {
		u := list.Items[i].DeepCopy()
		go a.handleEvent(ctx, "LIST", u)
	}
	// Watch
	listOpts.ResourceVersion = list.GetResourceVersion()
	w, err := a.naRes.Namespace(metav1.NamespaceAll).Watch(ctx, listOpts)
	if err != nil {
		return fmt.Errorf("watch NodeActions: %w", err)
	}
	for ev := range w.ResultChan() {
		if ev.Type == watch.Error {
			// watch error; break and let caller restart (or just continue)
			log.Printf("watch error: %#v", ev.Object)
			continue
		}
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			// as table? ignore
			continue
		}
		a.handleEvent(ctx, string(ev.Type), u.DeepCopy())
	}
	return nil
}

func (a *Agent) handleEvent(ctx context.Context, _ string, u *unstructured.Unstructured) {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if phase != "" && phase != "Pending" {
		return
	}
	// Acquire semaphore
	a.semaphore <- struct{}{}
	go func() {
		defer func() { <-a.semaphore }()
		if err := a.processOne(ctx, u); err != nil {
			log.Printf("process %s/%s: %v", u.GetNamespace(), u.GetName(), err)
		}
	}()
}

type instructionRef struct {
	Name        string
	Runtime     string
	Instruction string
}

func (a *Agent) processOne(ctx context.Context, u *unstructured.Unstructured) error {
	ns, name := u.GetNamespace(), u.GetName()
	// Reload before claim
	fresh, err := a.naRes.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get NodeAction: %w", err)
	}
	// Only handle Pending
	curPhase, _, _ := unstructured.NestedString(fresh.Object, "status", "phase")
	if curPhase != "" && curPhase != "Pending" {
		return nil
	}

	// Claim: phase -> Running, startedAt
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = unstructured.SetNestedField(fresh.Object, "Running", "status", "phase")
	_ = unstructured.SetNestedField(fresh.Object, now, "status", "startedAt")
	// Ensure conditions array exists
	if _, found, _ := unstructured.NestedSlice(fresh.Object, "status", "conditions"); !found {
		_ = unstructured.SetNestedSlice(fresh.Object, []interface{}{}, "status", "conditions")
	}
	// Update status
	var claimed *unstructured.Unstructured
	for i := 0; i < 3; i++ {
		claimed, err = a.naRes.Namespace(ns).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
		fresh, _ = a.naRes.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		_ = unstructured.SetNestedField(fresh.Object, "Running", "status", "phase")
		_ = unstructured.SetNestedField(fresh.Object, now, "status", "startedAt")
	}
	if err != nil {
		return fmt.Errorf("claim running: %w", err)
	}

	a.recorder.Eventf(claimed, corev1.EventTypeNormal, "Executing", "Execution started on node %s", a.cfg.NodeName)

	// Gather inputs
	nodeName, _, _ := unstructured.NestedString(claimed.Object, "spec", "nodeName")
	if nodeName != a.cfg.NodeName {
		return nil // shouldn't happen due to fieldSelector
	}
	timeoutSeconds, _ := nestedInt64(claimed.Object, "spec", "timeoutSeconds")
	subjectID, _, _ := unstructured.NestedString(claimed.Object, "spec", "resolvedSubjectID")
	executionID, _, _ := unstructured.NestedString(claimed.Object, "spec", "executionID")
	ir := instructionRef{
		Name:        nestedStringOr(claimed.Object, "", "spec", "instructionRef", "name"),
		Runtime:     nestedStringOr(claimed.Object, "", "spec", "instructionRef", "runtime"),
		Instruction: nestedStringOr(claimed.Object, "", "spec", "instructionRef", "instruction"),
	}
	params, _, _ := unstructured.NestedMap(claimed.Object, "spec", "params")

	// Fetch InstructionSet (namespaced or cluster-scoped). Assume namespaced same as NodeAction; fallback to cluster.
	isObj, err := a.isRes.Namespace(ns).Get(ctx, ir.Name, metav1.GetOptions{})
	if err != nil {
		// try cluster-scoped
		isObj, err = a.isRes.Namespace("").Get(ctx, ir.Name, metav1.GetOptions{})
		if err != nil {
			return a.finishWithError(ctx, claimed, fmt.Errorf("get InstructionSet %s: %w", ir.Name, err))
		}
	}

	// Param whitelist via JSONSchema properties (if provided)
	allowed := schemaAllowedParams(isObj)
	filteredParams := filterParams(params, allowed)

	// Secret injection support (valueFrom.secretKeyRef)
	injected, cleanup, err := a.injectSecrets(ctx, ns, filteredParams)
	if err != nil {
		return a.finishWithError(ctx, claimed, fmt.Errorf("inject secrets: %w", err))
	}
	defer cleanup()

	// Execute with timeout
	var execCtx context.Context
	var cancel context.CancelFunc
	if timeoutSeconds > 0 {
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	start := time.Now()
	activeNodeactions.WithLabelValues(ir.Runtime, ir.Instruction).Inc()
	var (
		phase     = "Failed"
		reason    = "Error"
		success   = "false"
		stdout    string
		stderr    string
		artifacts map[string]any
		exitCode  = -1
		timedOut  bool
	)

    switch strings.ToLower(ir.Runtime) {
    case "exec":
        // Render exec template locally, but delegate execution to gRPC driver.
        if a.grpcDriver == nil {
            return a.finishWithError(ctx, claimed, errors.New("grpc driver not configured (DRIVER_ADDR)"))
        }
        // Optional: fetch Node annotations/labels for template.
        node, err := a.clientset.CoreV1().Nodes().Get(ctx, a.cfg.NodeName, metav1.GetOptions{})
        if err != nil {
            node = &corev1.Node{}
        }
        // Resolve and render execTemplate.command []string
        cmdTmpls, err := extractCommandTemplates(isObj, ir.Runtime, ir.Instruction)
        if err != nil {
            return a.finishWithError(ctx, claimed, fmt.Errorf("extract execTemplate: %w", err))
        }
        data := map[string]any{
            "SubjectID": subjectID,
            "Params":    injected,
            "Node": map[string]any{
                "Name":        node.Name,
                "Labels":      node.Labels,
                "Annotations": node.Annotations,
            },
            "Annotations": claimed.GetAnnotations(),
        }
        argv, err := tpl.RenderCommandTemplates(cmdTmpls, data)
        if err != nil {
            return a.finishWithError(ctx, claimed, fmt.Errorf("render command: %w", err))
        }
        // Pass rendered argv to driver via special param `_argv` (JSON array string).
        sparams := stringifyParams(injected)
        if b, err := json.Marshal(argv); err == nil {
            sparams["_argv"] = string(b)
        }
        r, err := a.grpcDriver.Execute(execCtx, ir.Instruction, subjectID, executionID, sparams)
        if err != nil {
            if errors.Is(err, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
                timedOut = true
            } else {
                return a.finishWithError(ctx, claimed, fmt.Errorf("driver execute: %w", err))
            }
        } else {
            stdout = r.StdoutTail
            stderr = r.StderrTail
            exitCode = int(r.ExitCode)
            if r.Artifacts != nil {
                ma := make(map[string]any, len(r.Artifacts))
                for k, v := range r.Artifacts {
                    ma[k] = v
                }
                artifacts = ma
            }
        }
    default:
        if a.grpcDriver == nil {
            return a.finishWithError(ctx, claimed, errors.New("grpc driver not configured (DRIVER_ADDR)"))
        }
		// stringify injected params to strings
		sparams := stringifyParams(injected)
		r, err := a.grpcDriver.Execute(execCtx, ir.Instruction, subjectID, executionID, sparams)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
				timedOut = true
			} else {
				return a.finishWithError(ctx, claimed, fmt.Errorf("driver execute: %w", err))
			}
		} else {
			stdout = r.StdoutTail
			stderr = r.StderrTail
			exitCode = int(r.ExitCode)
			if r.Artifacts != nil {
				ma := make(map[string]any, len(r.Artifacts))
				for k, v := range r.Artifacts {
					ma[k] = v
				}
				artifacts = ma
			}
		}
	}

	activeNodeactions.WithLabelValues(ir.Runtime, ir.Instruction).Dec()
	actionDuration.WithLabelValues(ir.Runtime, ir.Instruction).Observe(time.Since(start).Seconds())

	if timedOut {
		reason = "Timeout"
	} else if exitCode == 0 {
		phase = "Succeeded"
		reason = "ExitCode0"
		success = "true"
	} else if exitCode >= 0 {
		reason = fmt.Sprintf("ExitCode%d", exitCode)
	}

	// Write back status
	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	patch := map[string]any{
		"status": map[string]any{
			"phase":      phase,
			"finishedAt": finishedAt,
			"result": map[string]any{
				"stdoutTail": stdout,
				"stderrTail": stderr,
				"artifacts":  artifacts,
			},
			"conditions": []interface{}{
				map[string]any{
					"type":               "Executed",
					"status":             "True",
					"reason":             reason,
					"lastTransitionTime": finishedAt,
				},
			},
		},
	}
	// Use UpdateStatus with fetched object
	latest, err := a.naRes.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		mergeStatus(latest, patch)
		_, _ = a.naRes.Namespace(ns).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
	}

	a.recorder.Eventf(claimed, corev1.EventTypeNormal, map[bool]string{true: "Completed", false: "Failed"}[phase == "Succeeded"], "Result: %s", reason)
	actionSuccess.WithLabelValues(ir.Runtime, ir.Instruction, strconv.Itoa(exitCode), success).Inc()
	return nil
}

func (a *Agent) finishWithError(ctx context.Context, u *unstructured.Unstructured, e error) error {
	ns, name := u.GetNamespace(), u.GetName()
	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	patch := map[string]any{
		"status": map[string]any{
			"phase":      "Failed",
			"finishedAt": finishedAt,
			"result": map[string]any{
				"stderrTail": truncate(fmt.Sprintf("%v", e), 4096),
			},
			"conditions": []interface{}{
				map[string]any{
					"type":               "Executed",
					"status":             "True",
					"reason":             "Error",
					"lastTransitionTime": finishedAt,
				},
			},
		},
	}
	latest, err := a.naRes.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		mergeStatus(latest, patch)
		_, _ = a.naRes.Namespace(ns).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
	}
	a.recorder.Eventf(u, corev1.EventTypeWarning, "Failed", "%v", e)
	return e
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func nestedInt64(obj map[string]any, fields ...string) (int64, bool) {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case json.Number:
		i, _ := t.Int64()
		return i, true
	default:
		return 0, false
	}
}

func nestedStringOr(obj map[string]any, def string, fields ...string) string {
	s, found, _ := unstructured.NestedString(obj, fields...)
	if !found {
		return def
	}
	return s
}

// extractCommandTemplates looks for spec.execTemplates[runtime][instruction].command (preferred),
// falling back to spec.instructions[...].execTemplate.command if present.
func extractCommandTemplates(is *unstructured.Unstructured, runtime, instruction string) ([]string, error) {
	// Preferred: nested maps
	if m1, found, _ := unstructured.NestedMap(is.Object, "spec", "execTemplates", runtime, instruction); found {
		if cmds, ok := m1["command"]; ok {
			if arr, ok := cmds.([]any); ok {
				return toStringSlice(arr)
			}
		}
	}
	// Fallback: iterate spec.instructions (array of objects)
	if arr, found, _ := unstructured.NestedSlice(is.Object, "spec", "instructions"); found {
		for _, it := range arr {
			if mm, ok := it.(map[string]any); ok {
				r := nestedStringOr(mm, "", "runtime")
				in := nestedStringOr(mm, "", "instruction")
				if r == runtime && in == instruction {
					if m, found, _ := unstructured.NestedMap(mm, "execTemplate"); found {
						if cmds, ok := m["command"]; ok {
							if a, ok := cmds.([]any); ok {
								return toStringSlice(a)
							}
						}
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("execTemplate.command not found for runtime=%s instruction=%s", runtime, instruction)
}

func toStringSlice(a []any) ([]string, error) {
	out := make([]string, 0, len(a))
	for i, v := range a {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("command[%d] not a string", i)
		}
		out = append(out, s)
	}
	return out, nil
}

// schemaAllowedParams extracts allowed param keys from spec.paramsSchema.properties
func schemaAllowedParams(is *unstructured.Unstructured) map[string]struct{} {
	props, found, _ := unstructured.NestedMap(is.Object, "spec", "paramsSchema", "properties")
	if !found {
		return nil
	}
	out := make(map[string]struct{}, len(props))
	for k := range props {
		out[k] = struct{}{}
	}
	return out
}

func filterParams(params map[string]any, allowed map[string]struct{}) map[string]any {
	if params == nil {
		return nil
	}
	if allowed == nil {
		// no schema => allow all
		return params
	}
	out := map[string]any{}
	for k, v := range params {
		if _, ok := allowed[k]; ok {
			out[k] = v
		}
	}
	return out
}

// injectSecrets scans params and resolves entries like {valueFrom: {secretKeyRef: {name, key}}}.
// It writes the secret value to a temp file and replaces the param with the temp filepath.
func (a *Agent) injectSecrets(ctx context.Context, ns string, params map[string]any) (map[string]any, func(), error) {
	if params == nil {
		return nil, func() {}, nil
	}
	out := map[string]any{}
	var files []string
	cleanup := func() {
		for _, f := range files {
			_ = os.Remove(f)
		}
	}
	for k, v := range params {
		m, ok := v.(map[string]any)
		if !ok {
			out[k] = v
			continue
		}
		vf, ok := m["valueFrom"].(map[string]any)
		if !ok {
			out[k] = v
			continue
		}
		skr, ok := vf["secretKeyRef"].(map[string]any)
		if !ok {
			out[k] = v
			continue
		}
		sname := nestedStringOr(skr, "", "name")
		skey := nestedStringOr(skr, "", "key")
		if sname == "" || skey == "" {
			out[k] = v
			continue
		}
		sec, err := a.clientset.CoreV1().Secrets(ns).Get(ctx, sname, metav1.GetOptions{})
		if err != nil {
			return nil, cleanup, fmt.Errorf("get secret %s: %w", sname, err)
		}
		val, ok := sec.Data[skey]
		if !ok {
			return nil, cleanup, fmt.Errorf("secret %s key %s not found", sname, skey)
		}
		// write to temp file
		dir := os.TempDir()
		path := filepath.Join(dir, fmt.Sprintf("na-secret-%s-%s", sname, sanitizeFileName(skey)))
		if err := os.WriteFile(path, val, 0600); err != nil {
			return nil, cleanup, fmt.Errorf("write secret temp file: %w", err)
		}
		files = append(files, path)
		out[k] = path
	}
	return out, cleanup, nil
}

func sanitizeFileName(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "..", "_")
	return s
}

func mergeStatus(dst *unstructured.Unstructured, src map[string]any) {
	// naive merge for status fields used by this agent
	if status, ok := src["status"].(map[string]any); ok {
		for k, v := range status {
			_ = unstructured.SetNestedField(dst.Object, v, "status", k)
		}
	}
}

func stringifyParams(params map[string]any) map[string]string {
	if params == nil {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case int:
			out[k] = strconv.Itoa(t)
		case int64:
			out[k] = strconv.FormatInt(t, 10)
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		default:
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out
}
