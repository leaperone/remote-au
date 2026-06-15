package audio

// #include <stdlib.h>
import "C"
import "unsafe"

func freeDeviceIDPointer(ptr unsafe.Pointer) {
	C.free(ptr)
}
