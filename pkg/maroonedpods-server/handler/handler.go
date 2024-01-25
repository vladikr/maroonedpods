package handler

import (
	"encoding/json"
	"fmt"
	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"maroonedpods.io/maroonedpods/pkg/util"
	"net/http"
	"strings"
)

const (
	allowPodRequest               = "Pod has successfully gated"
	validPodUpdate                = "Pod update did not remove MaroonedPodsGate"
	maroonedpodsControllerPodUpdate        = "MaroonedPods controller has permission to remove gate from pods"
	invalidPodUpdate              = "Only MaroonedPods controller has permission to remove " + util.MaroonedPodsGate + " gate from pods"
)

type Handler struct {
	request *admissionv1.AdmissionRequest
	maroonedpodsCli  kubernetes.Interface
	maroonedpodsNS   string
}

func NewHandler(Request *admissionv1.AdmissionRequest, maroonedpodsCli kubernetes.Interface, maroonedpodsNS string) *Handler {
	return &Handler{
		request: Request,
		maroonedpodsCli:  maroonedpodsCli,
		maroonedpodsNS:   maroonedpodsNS,
	}
}

func (v Handler) Handle() (*admissionv1.AdmissionReview, error) {
	if v.shouldMutate() {
		return v.mutatePod()
	}

	switch v.request.Kind.Kind {
	case "Pod":
		return v.validatePodUpdate()
	}
	return nil, fmt.Errorf("MaroonedPods webhook doesn't recongnize request: %+v", v.request)
}

func (v Handler) shouldMutate() bool {
	return v.request.Kind.Kind == "Pod" && v.request.Operation == admissionv1.Create
}

func (v Handler) mutatePod() (*admissionv1.AdmissionReview, error) {
	pod := v1.Pod{}
	if err := json.Unmarshal(v.request.Object.Raw, &pod); err != nil {
		return nil, err
	}
	schedulingGates := pod.Spec.SchedulingGates
	if schedulingGates == nil {
		schedulingGates = []v1.PodSchedulingGate{}
	}
	schedulingGates = append(schedulingGates, v1.PodSchedulingGate{Name: util.MaroonedPodsGate})

	schedulingGatesBytes, err := json.Marshal(schedulingGates)
	if err != nil {
		return nil, err
	}

	patch := fmt.Sprintf(`[{"op": "add", "path": "/spec/schedulingGates", "value": %s}]`, string(schedulingGatesBytes))
	return reviewResponseWithPatch(v.request.UID, true, http.StatusAccepted, allowPodRequest, []byte(patch)), nil
}

func reviewResponseWithPatch(uid types.UID, allowed bool, httpCode int32,
	reason string, patch []byte) *admissionv1.AdmissionReview {
	rr := reviewResponse(uid, allowed, httpCode, reason)
	patchType := admissionv1.PatchTypeJSONPatch
	rr.Response.PatchType = &patchType
	rr.Response.Patch = patch
	return rr
}

func reviewResponse(uid types.UID, allowed bool, httpCode int32,
	reason string) *admissionv1.AdmissionReview {
	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     uid,
			Allowed: allowed,
			Result: &metav1.Status{
				Code:    httpCode,
				Message: reason,
			},
		},
	}
}


func (v Handler) validatePodUpdate() (*admissionv1.AdmissionReview, error) {
	oldPod := v1.Pod{}
	if err := json.Unmarshal(v.request.OldObject.Raw, &oldPod); err != nil {
		return nil, err
	}

	if !hasMaroonedPodsGate(oldPod.Spec.SchedulingGates) {
		return reviewResponse(v.request.UID, true, http.StatusAccepted, validPodUpdate), nil
	}

	currentPod := v1.Pod{}
	if err := json.Unmarshal(v.request.Object.Raw, &currentPod); err != nil {
		return nil, err
	}

	if hasMaroonedPodsGate(currentPod.Spec.SchedulingGates) {
		return reviewResponse(v.request.UID, true, http.StatusAccepted, validPodUpdate), nil
	}

	if isMaroonedPodsControllerServiceAccount(v.request.UserInfo.Username, v.maroonedpodsNS) {
		return reviewResponse(v.request.UID, true, http.StatusAccepted, maroonedpodsControllerPodUpdate), nil
	}

	return reviewResponse(v.request.UID, false, http.StatusForbidden, invalidPodUpdate), nil

}

func hasMaroonedPodsGate(psgs []v1.PodSchedulingGate) bool {
	if psgs == nil {
		return false
	}
	for _, sg := range psgs {
		if sg.Name == util.MaroonedPodsGate {
			return true
		}
	}
	return false
}


func ignoreRqErr(err string) string {
	return strings.TrimPrefix(err, strings.Split(err, ":")[0]+": ")
}

func isMaroonedPodsControllerServiceAccount(serviceAccount string, maroonedpodsNS string) bool {
	prefix := fmt.Sprintf("system:serviceaccount:%s", maroonedpodsNS)
	return serviceAccount == fmt.Sprintf("%s:%s", prefix, util.ControllerResourceName)
}
