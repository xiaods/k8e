package main

import (
	"fmt"
	"os"

	//	"k8s.io/apiserver/pkg/admission/plugin/webhook/config"

	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/agent"
	"github.com/xiaods/k8e/pkg/version"
	"k8s.io/apimachinery/pkg/util/net"
)

func testGetHostFileClientCA() error {
	info, err := clientaccess.ParseAndValidateToken("http://127.0.0.1:8081", "")
	fileBytes, err := clientaccess.Get("/v1-"+version.Program+"/client-ca.crt"+"", info)
	if err != nil {
		return err
	}
	fmt.Println(string(fileBytes))
	fileBytes, err = clientaccess.Get("/v1-"+version.Program+"/server-ca.crt"+"", info)
	if err != nil {
		return err
	}
	fmt.Println(string(fileBytes))
	return nil
}

func testGetHostFileKubelet() error {

	hostIP, err := net.ChooseHostInterface()
	if err != nil {
		return err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	info, err := clientaccess.ParseAndValidateToken("http://127.0.0.1:8081", "")
	fileBytes, err := agent.Request("/v1-"+version.Program+"/"+"client-kubelet.crt", info, agent.GetNodeNamedCrt(hostname, hostIP.String(), ""))
	if err != nil {
		return err
	}
	fileBytes, keyBytes := agent.SplitCertKeyPEM(fileBytes)
	fmt.Println(string(fileBytes))
	fmt.Println(string(keyBytes))
	return nil
}

func main() {
	fmt.Println("get client ca")
	fmt.Println(testGetHostFileClientCA())
	fmt.Println("----------------------------")
	fmt.Println("get client kubelet cert")
	fmt.Println(testGetHostFileKubelet())

}
