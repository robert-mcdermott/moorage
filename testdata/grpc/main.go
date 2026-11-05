// A gRPC server with only the health service, which needs no generated code: CI deploys it, and asks it how it is.
package main

import (
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	listener, err := net.Listen("tcp", os.Getenv("HOST")+":"+os.Getenv("PORT"))
	if err != nil {
		log.Fatal(err)
	}
	status := health.NewServer()
	status.SetServingStatus("drill", healthpb.HealthCheckResponse_SERVING)
	server := grpc.NewServer()
	healthpb.RegisterHealthServer(server, status)
	log.Println("serving gRPC on", listener.Addr())
	log.Fatal(server.Serve(listener))
}
