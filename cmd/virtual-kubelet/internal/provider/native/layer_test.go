package native

import (
	"fmt"
	"testing"
)

func TestUnTar(t *testing.T) {
	err := UnTar("/home/tx/native-kubelet/containers/default/nginx/nginx/image/2c730127189a31a95f867df51a494f1887e2ee052743a87c08196be8c2ecdc8e",
		"/home/tx/native-kubelet/containers/default/nginx/nginx/container")
	fmt.Println(err)
}

//func TestUnTar2(t *testing.T) {
//	err := untar("/home/tx/native-kubelet/containers/default/nginx/nginx/image/2c730127189a31a95f867df51a494f1887e2ee052743a87c08196be8c2ecdc8e",
//		"/home/tx/native-kubelet/containers/default/nginx/nginx/container")
//	fmt.Println(err)
//}
