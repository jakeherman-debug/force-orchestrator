// Go gRPC client fixture for D15 P5 consumer extractor tests.

package userclient

import (
	"context"

	pb "example.com/proto/user"
	"google.golang.org/grpc"
)

// UserClient wraps the generated gRPC stub.
type UserClient struct {
	client pb.UserServiceClient
}

// NewUserClient dials the gRPC server and returns a wrapper.
func NewUserClient(cc *grpc.ClientConn) *UserClient {
	client := pb.NewUserServiceClient(cc)
	return &UserClient{client: client}
}

// GetUser fetches a user by ID.
func (u *UserClient) GetUser(ctx context.Context, id string) (*pb.User, error) {
	resp, err := client.GetUser(ctx, &pb.GetUserRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateUser creates a new user.
func (u *UserClient) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.User, error) {
	resp, err := client.CreateUser(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
