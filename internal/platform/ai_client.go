package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"

	visionpb "iot-vision-platform/internal/visionpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type VisionAnalyzer interface {
	Analyze(ctx context.Context, req AnalyzeImageRequest) ([]GeneratedTag, error)
	Close() error
}

type AnalyzeImageRequest struct {
	RequestID   string
	ImageID     string
	ImageURI    string
	Filename    string
	ContentType string
	Tasks       []AnalysisTask
	Params      map[string]string
}

type Detection struct {
	Label      string
	Confidence float64
	Box        BoundingBox
}

type BoundingBox struct {
	X              float64
	Y              float64
	Width          float64
	Height         float64
	CoordinateType string
}

type AnalysisTask struct {
	Type   string
	Params map[string]string
}

type GRPCVisionAnalyzer struct {
	conn   *grpc.ClientConn
	client visionpb.VisionAnalysisServiceClient
}

func NewGRPCVisionAnalyzer(addr string) (*GRPCVisionAnalyzer, error) {
	conn, err := grpc.NewClient(
		grpcTarget(addr),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		return nil, err
	}
	return &GRPCVisionAnalyzer{
		conn:   conn,
		client: visionpb.NewVisionAnalysisServiceClient(conn),
	}, nil
}

func (a *GRPCVisionAnalyzer) Analyze(ctx context.Context, req AnalyzeImageRequest) ([]GeneratedTag, error) {
	if len(req.Tasks) == 0 {
		req.Tasks = []AnalysisTask{{Type: "classification"}}
	}
	resp, err := a.client.AnalyzeImage(ctx, toProtoAnalyzeRequest(req))
	if err != nil {
		return nil, err
	}
	tags := []GeneratedTag{}
	for _, output := range resp.GetOutputs() {
		if output.GetType() != "classification" || output.GetClassification() == nil {
			continue
		}
		for _, label := range output.GetClassification().GetLabels() {
			tags = append(tags, GeneratedTag{Tag: label.GetTag(), Confidence: label.GetConfidence()})
		}
	}
	if len(tags) == 0 {
		return nil, errors.New("ai service returned no classification labels")
	}
	return dedupeTags(tags), nil
}

func (a *GRPCVisionAnalyzer) Detect(ctx context.Context, req AnalyzeImageRequest) ([]Detection, error) {
	req.Tasks = []AnalysisTask{{Type: "detection"}}
	resp, err := a.client.AnalyzeImage(ctx, toProtoAnalyzeRequest(req))
	if err != nil {
		return nil, err
	}
	detections := []Detection{}
	for _, output := range resp.GetOutputs() {
		if output.GetType() != "detection" || output.GetDetection() == nil {
			continue
		}
		for _, item := range output.GetDetection().GetDetections() {
			box := item.GetBox()
			if box == nil {
				continue
			}
			detections = append(detections, Detection{
				Label:      item.GetLabel(),
				Confidence: item.GetConfidence(),
				Box: BoundingBox{
					X:              box.GetX(),
					Y:              box.GetY(),
					Width:          box.GetWidth(),
					Height:         box.GetHeight(),
					CoordinateType: box.GetCoordinateType(),
				},
			})
		}
	}
	return detections, nil
}

func (a *GRPCVisionAnalyzer) Close() error {
	if a == nil || a.conn == nil {
		return nil
	}
	return a.conn.Close()
}

func toProtoAnalyzeRequest(req AnalyzeImageRequest) *visionpb.AnalyzeImageRequest {
	tasks := make([]*visionpb.AnalysisTask, 0, len(req.Tasks))
	for _, task := range req.Tasks {
		tasks = append(tasks, &visionpb.AnalysisTask{
			Type:   task.Type,
			Params: task.Params,
		})
	}
	return &visionpb.AnalyzeImageRequest{
		RequestId:   req.RequestID,
		ImageId:     req.ImageID,
		ImageUri:    req.ImageURI,
		Filename:    req.Filename,
		ContentType: req.ContentType,
		Tasks:       tasks,
		Params:      req.Params,
	}
}

func grpcTarget(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "127.0.0.1:9000"
	}
	if strings.Contains(addr, ":///") {
		return addr
	}
	return fmt.Sprintf("dns:///%s", addr)
}
