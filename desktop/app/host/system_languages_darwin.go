//go:build darwin && cgo

package host

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

static void *displaykvm_preferred_languages(void) {
	return (void *)CFLocaleCopyPreferredLanguages();
}

static CFIndex displaykvm_language_count(void *languages) {
	return CFArrayGetCount((CFArrayRef)languages);
}

static char *displaykvm_copy_language(void *languages, CFIndex index) {
	CFTypeRef value = CFArrayGetValueAtIndex((CFArrayRef)languages, index);
	if (!value || CFGetTypeID(value) != CFStringGetTypeID()) return NULL;
	CFStringRef language = (CFStringRef)value;
	CFIndex capacity = CFStringGetMaximumSizeForEncoding(CFStringGetLength(language), kCFStringEncodingUTF8) + 1;
	if (capacity <= 1 || capacity > 1024 * 1024) return NULL;
	char *buffer = (char *)malloc((size_t)capacity);
	if (buffer && !CFStringGetCString(language, buffer, capacity, kCFStringEncodingUTF8)) {
		free(buffer);
		return NULL;
	}
	return buffer;
}

static void displaykvm_release_languages(void *languages) {
	CFRelease((CFArrayRef)languages);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

func platformLanguages() ([]string, error) {
	languages := C.displaykvm_preferred_languages()
	if languages == nil {
		return nil, errors.New("CFLocaleCopyPreferredLanguages未返回首选语言")
	}
	// Copy拥有的CFArray必须保留到所有字符串复制进Go之后再释放；单个UTF-8
	// 缓冲在复制后立即free，Go返回值不持有任何CoreFoundation或C内存。
	defer C.displaykvm_release_languages(languages)
	count := C.displaykvm_language_count(languages)
	if count <= 0 || count > 1024 {
		return nil, fmt.Errorf("macOS首选语言数量无效：%d", count)
	}
	preferred := make([]string, 0, int(count))
	for index := C.CFIndex(0); index < count; index++ {
		buffer := C.displaykvm_copy_language(languages, index)
		if buffer == nil {
			return nil, fmt.Errorf("macOS首选语言%d无法复制为UTF-8", index)
		}
		preferred = append(preferred, C.GoString(buffer))
		C.free(unsafe.Pointer(buffer))
	}
	return preferred, nil
}
