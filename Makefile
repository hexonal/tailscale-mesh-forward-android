# Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

## For signed release build JKS_PASSWORD must be set to the password for the jks keystore
## and JKS_PATH must be set to the path to the jks keystore.

# The docker image to use for the build environment.  Changing this
# will force a rebuild of the docker image.  If there is an existing image
# with this name, it will be used.
#
# The convention here is tailscale-android-build-amd64-<date>
DOCKER_IMAGE := tailscale-android-build-amd64-041425-1
export TS_USE_TOOLCHAIN=1

# Auto-select an NDK from ANDROID_HOME (choose highest version available)
NDK_ROOT ?= $(shell ls -1d $(ANDROID_HOME)/ndk/* 2>/dev/null | sort -V | tail -n 1)

HOST_OS := $(shell uname | tr A-Z a-z)
ifeq ($(HOST_OS),linux)
    STRIP_TOOL := $(NDK_ROOT)/toolchains/llvm/prebuilt/linux-x86_64/bin/llvm-objcopy
else ifeq ($(HOST_OS),darwin)
    STRIP_TOOL := $(NDK_ROOT)/toolchains/llvm/prebuilt/darwin-x86_64/bin/llvm-objcopy
endif

$(info Using NDK_ROOT: $(NDK_ROOT))
$(info Using STRIP_TOOL: $(STRIP_TOOL))

DEBUG_APK := tailscale-debug.apk
RELEASE_APK := tailscale-release.apk
RELEASE_AAB := tailscale-release.aab
RELEASE_TV_AAB := tailscale-tv-release.aab

# Define output filenames.
LIBTAILSCALE_AAR := android/libs/libtailscale.aar
UNSTRIPPED_AAR := android/libs/libtailscale_unstripped.aar
ARM64_SO_PATH := jni/arm64-v8a/libgojni.so

# Compute an absolute path for the unstripped AAR.
ABS_UNSTRIPPED_AAR := $(shell pwd)/$(UNSTRIPPED_AAR)

# Android SDK & Tools settings.
ifeq ($(shell uname),Linux)
    ANDROID_TOOLS_URL := "https://dl.google.com/android/repository/commandlinetools-linux-9477386_latest.zip"
#    ANDROID_TOOLS_SUM := "bd1aa17c7ef10066949c88dc6c9c8d536be27f992a1f3b5a584f9bd2ba5646a0  commandlinetools-linux-9477386_latest.zip"
else
    ANDROID_TOOLS_URL := "https://dl.google.com/android/repository/commandlinetools-mac-9477386_latest.zip"
#    ANDROID_TOOLS_SUM := "2072ffce4f54cdc0e6d2074d2f381e7e579b7d63e915c220b96a7db95b2900ee  commandlinetools-mac-9477386_latest.zip"
endif
ANDROID_SDK_PACKAGES := 'platforms;android-34' 'extras;android;m2repository' 'ndk;23.1.7779620' 'platform-tools' 'build-tools;34.0.0'

# Attempt to find an ANDROID_SDK_ROOT / ANDROID_HOME based either from
# preexisting environment or common locations.
export ANDROID_SDK_ROOT ?= $(shell find $$ANDROID_SDK_ROOT $$ANDROID_HOME $$HOME/Library/Android/sdk $$HOME/Android/Sdk $$HOME/AppData/Local/Android/Sdk /usr/lib/android-sdk -maxdepth 1 -type d 2>/dev/null | head -n 1)
ifeq ($(ANDROID_SDK_ROOT),)
    ifeq ($(shell uname),Linux)
        export ANDROID_SDK_ROOT := $(HOME)/Android/Sdk
    else ifeq ($(shell uname),Darwin)
        export ANDROID_SDK_ROOT := $(HOME)/Library/Android/sdk
    else ifneq ($(WINDIR),)
        export ANDROID_SDK_ROOT := $(HOME)/AppData/Local/Android/sdk
    else
        export ANDROID_SDK_ROOT := $(PWD)/android-sdk
    endif
endif
export ANDROID_HOME ?= $(ANDROID_SDK_ROOT)

# Attempt to find Android Studio for Linux configuration, which does not have a
# predetermined location.
ANDROID_STUDIO_ROOT ?= $(shell find ~/android-studio /usr/local/android-studio /opt/android-studio /Applications/Android\ Studio.app $(PROGRAMFILES)/Android/Android\ Studio -type d -maxdepth 1 2>/dev/null | head -n 1)

# Set JAVA_HOME to the Android Studio bundled JDK.
export JAVA_HOME ?= $(shell find "$(ANDROID_STUDIO_ROOT)/jbr" "$(ANDROID_STUDIO_ROOT)/jre" "$(ANDROID_STUDIO_ROOT)/Contents/jbr/Contents/Home" "$(ANDROID_STUDIO_ROOT)/Contents/jre/Contents/Home" -maxdepth 1 -type d 2>/dev/null | head -n 1)
ifeq ($(JAVA_HOME),)
    unexport JAVA_HOME
else
    export PATH := $(JAVA_HOME)/bin:$(PATH)
endif

AVD_BASE_IMAGE := "system-images;android-33;google_apis;"
export HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),aarch64)
    AVD_IMAGE := "$(AVD_BASE_IMAGE)arm64-v8a"
else ifeq ($(HOST_ARCH),arm64)
    AVD_IMAGE := "$(AVD_BASE_IMAGE)arm64-v8a"
else
    AVD_IMAGE := "$(AVD_BASE_IMAGE)x86_64"
endif
AVD ?= tailscale-$(HOST_ARCH)
export AVD_IMAGE
export AVD

# Use our toolchain or the one that is specified, do not perform dynamic toolchain switching.
GOTOOLCHAIN := local
export GOTOOLCHAIN

TOOLCHAINDIR ?=
export TOOLCHAINDIR

GOBIN := $(PWD)/android/build/go/bin
export GOBIN

export PATH := $(PWD)/tool:$(GOBIN):$(ANDROID_HOME)/cmdline-tools/latest/bin:$(ANDROID_HOME)/platform-tools:$(PATH)
export GOROOT := # Unset

# ------------------------------------------------------------------------------
# Android Build Targets
# ------------------------------------------------------------------------------


.PHONY: debug-unstripped
debug-unstripped: build-unstripped-aar
	@echo "Listing contents of $(ABS_UNSTRIPPED_AAR):"
	unzip -l $(ABS_UNSTRIPPED_AAR)

.PHONY: apk
apk: $(DEBUG_APK)

.PHONY: tailscale-debug
tailscale-debug: $(DEBUG_APK)

$(DEBUG_APK): libtailscale debug-symbols version gradle-dependencies build-unstripped-aar
	(cd android && ./gradlew test assembleDebug)
	install -C android/build/outputs/apk/debug/android-debug.apk $@

.PHONY: tailscale-release
tailscale-release: $(RELEASE_APK)

$(RELEASE_APK): version gradle-dependencies
	@echo "Building release APK"
	(cd android && ./gradlew test assembleRelease)
	install -C android/build/outputs/apk/release/android-release.apk $@

# Builds the release AAB and signs it (phone/tablet/chromeOS variant)
.PHONY: release
release: jarsign-env $(RELEASE_AAB)
	@jarsigner -sigalg SHA256withRSA -digestalg SHA-256 -keystore $(JKS_PATH) -storepass $(JKS_PASSWORD) $(RELEASE_AAB) tailscale

# Builds the release AAB and signs it (androidTV variant)
.PHONY: release-tv
release-tv: jarsign-env $(RELEASE_TV_AAB)
	@jarsigner -sigalg SHA256withRSA -digestalg SHA-256 -keystore $(JKS_PATH) -storepass $(JKS_PASSWORD) $(RELEASE_TV_AAB) tailscale

# gradle-dependencies groups together the android sources and libtailscale needed to assemble tests/debug/release builds.
.PHONY: gradle-dependencies
gradle-dependencies: $(shell find android -type f -not -path "android/build/*" -not -path '*/.*') $(LIBTAILSCALE_AAR) tailscale.version

$(RELEASE_AAB): version gradle-dependencies
	@echo "Building release AAB"
	(cd android && ./gradlew test bundleRelease)
	install -C ./android/build/outputs/bundle/release/android-release.aab $@

$(RELEASE_TV_AAB): version gradle-dependencies
	@echo "Building TV release AAB"
	(cd android && ./gradlew test bundleRelease_tv)
	install -C ./android/build/outputs/bundle/release_tv/android-release_tv.aab $@

tailscale-test.apk: version gradle-dependencies
	(cd android && ./gradlew assembleApplicationTestAndroidTest)
	install -C ./android/build/outputs/apk/androidTest/applicationTest/android-applicationTest-androidTest.apk $@

tailscale.version: go.mod go.sum $(wildcard .git/HEAD)
	@bash -c "./tool/go run tailscale.com/cmd/mkversion > tailscale.version"

.PHONY: version
version: tailscale.version
	@cat tailscale.version

# ------------------------------------------------------------------------------
# Go Build Targets (Unstripped AAR, Debug Symbols, Stripped SO, Packaging)
# ------------------------------------------------------------------------------

android/libs:
	mkdir -p android/libs

$(GOBIN):
	mkdir -p $(GOBIN)

$(GOBIN)/gomobile: $(GOBIN)/gobind go.mod go.sum | $(GOBIN)
	./tool/go install golang.org/x/mobile/cmd/gomobile

$(GOBIN)/gobind: go.mod go.sum
	./tool/go install golang.org/x/mobile/cmd/gobind

.PHONY: build-unstripped-aar
build-unstripped-aar: tailscale.version $(GOBIN)/gomobile
	@echo "Running gomobile bind to generate unstripped AAR..."
	@echo "Output file: $(ABS_UNSTRIPPED_AAR)"
	mkdir -p $(dir $(ABS_UNSTRIPPED_AAR))
	rm -f $(ABS_UNSTRIPPED_AAR)
	$(GOBIN)/gomobile bind -target android -androidapi 26 \
		-tags "$$(./build-tags.sh)" \
		-ldflags "$$(./version-ldflags.sh)" \
		-o $(ABS_UNSTRIPPED_AAR) ./libtailscale || { echo "gomobile bind failed"; exit 1; }
	@if [ ! -f $(ABS_UNSTRIPPED_AAR) ]; then \
	    echo "Error: $(ABS_UNSTRIPPED_AAR) was not created"; exit 1; \
	fi
	@echo "Generated unstripped AAR: $(ABS_UNSTRIPPED_AAR)"

$(UNSTRIPPED_AAR): build-unstripped-aar

libgojni.so.unstripped: $(UNSTRIPPED_AAR)
	@echo "Extracting libgojni.so from unstripped AAR..."
	@if unzip -p $(ABS_UNSTRIPPED_AAR) jni/arm64-v8a/libgojni.so > libgojni.so.unstripped; then \
	    echo "Found arm64-v8a libgojni.so"; \
	elif unzip -p $(ABS_UNSTRIPPED_AAR) jni/armeabi-v7a/libgojni.so > libgojni.so.unstripped; then \
	    echo "Found armeabi-v7a libgojni.so"; \
	else \
	    echo "Neither jni/arm64-v8a/libgojni.so nor jni/armeabi-v7a/libgojni.so was found."; \
	    echo "Listing contents of $(ABS_UNSTRIPPED_AAR):"; \
	    unzip -l $(ABS_UNSTRIPPED_AAR); exit 1; \
	fi

libgojni.so.debug: libgojni.so.unstripped
	@echo "Extracting debug symbols from libgojni.so..."
	$(STRIP_TOOL) --only-keep-debug libgojni.so.unstripped libgojni.so.debug

libgojni.so.stripped: libgojni.so.unstripped
	@echo "Stripping debug symbols from libgojni.so..."
	$(STRIP_TOOL) --strip-debug libgojni.so.unstripped libgojni.so.stripped

$(LIBTAILSCALE_AAR): libgojni.so.stripped $(UNSTRIPPED_AAR)
	@echo "Repackaging AAR with stripped libgojni.so..."
	rm -rf temp_aar
	mkdir temp_aar
	unzip $(ABS_UNSTRIPPED_AAR) -d temp_aar
	cp libgojni.so.stripped temp_aar/$(ARM64_SO_PATH)
	(cd temp_aar && zip -r ../$(LIBTAILSCALE_AAR) .)
	rm -rf temp_aar

.PHONY: libtailscale
libtailscale: $(LIBTAILSCALE_AAR) ## Build the stripped libtailscale AAR

.PHONY: debug-symbols
debug-symbols: libgojni.so.debug

# ------------------------------------------------------------------------------
# Utility Targets
# ------------------------------------------------------------------------------

.PHONY: env
env:
	@echo "PATH=$(PATH)"
	@echo "ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT)"
	@echo "ANDROID_HOME=$(ANDROID_HOME)"
	@echo "ANDROID_STUDIO_ROOT=$(ANDROID_STUDIO_ROOT)"
	@echo "JAVA_HOME=$(JAVA_HOME)"
	@echo "TOOLCHAINDIR=$(TOOLCHAINDIR)"
	@echo "AVD_IMAGE=$(AVD_IMAGE)"

# Ensure that JKS_PATH and JKS_PASSWORD are set before we attempt a build
# that requires signing.
.PHONY: jarsign-env
jarsign-env:
ifeq ($(JKS_PATH),)
	$(error JKS_PATH is not set.  export JKS_PATH=/path/to/tailcale.jks)
endif
ifeq ($(JKS_PASSWORD),)
	$(error JKS_PASSWORD is not set.  export JKS_PASSWORD=passwordForTailcale.jks)
endif
ifeq ($(wildcard $(JKS_PATH)),)
	$(error JKS_PATH does not point to a file)
endif
	@echo "keystore path set to $(JKS_PATH)"

.PHONY: androidpath
androidpath:
	@echo "export ANDROID_HOME=$(ANDROID_HOME)"
	@echo "export ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT)"
	@echo 'export PATH=$(ANDROID_HOME)/cmdline-tools/latest/bin:$(ANDROID_HOME)/platform-tools:$$PATH'

.PHONY: tag_release
tag_release: debug-symbols tailscale.version ## Tag the current commit with the current version
	source tailscale.version && git tag -a "$${VERSION_LONG}" -m "OSS and Version updated to $${VERSION_LONG}"

.PHONY: bumposs ## Bump to the latest oss and update the versions.
bumposs: update-oss tailscale.version
	source tailscale.version && git commit -sm "android: bump OSS" -m "OSS and Version updated to $${VERSION_LONG}" go.toolchain.rev android/build.gradle go.mod go.sum
	source tailscale.version && git tag -a "$${VERSION_LONG}" -m "OSS and Version updated to $${VERSION_LONG}"

.PHONY: bump_version_code ## Bump the version code in build.gradle
bump_version_code:
	sed -i'.bak' "s/versionCode .*/versionCode $$(expr $$(awk '/versionCode ([0-9]+)/{print $$2}' android/build.gradle) + 1)/" android/build.gradle && rm android/build.gradle.bak

.PHONY: update-oss ## Update the tailscale.com go module
update-oss:
	GOPROXY=direct ./tool/go get tailscale.com@main
	./tool/go mod tidy -compat=1.24
	./tool/go run tailscale.com/cmd/printdep --go > go.toolchain.rev.new
	mv go.toolchain.rev.new go.toolchain.rev

# Get the commandline tools package, this provides (among other things) the sdkmanager binary.
$(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager:
	mkdir -p $(ANDROID_HOME)/tmp
	mkdir -p $(ANDROID_HOME)/cmdline-tools
	(cd $(ANDROID_HOME)/tmp && \
		curl --silent -O -L $(ANDROID_TOOLS_URL) && \
		unzip $(shell basename $(ANDROID_TOOLS_URL)))
	mv $(ANDROID_HOME)/tmp/cmdline-tools $(ANDROID_HOME)/cmdline-tools/latest
	rm -rf $(ANDROID_HOME)/tmp

.PHONY: androidsdk
androidsdk: $(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager ## Install the set of Android SDK packages we need.
	yes | $(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager --licenses > /dev/null
	$(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager --update
	$(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager $(ANDROID_SDK_PACKAGES)

# Normally in make you would simply take a dependency on the task that provides
# the binaries, however users may have a decision to make as to whether they
# want to install an SDK or use the one from an Android Studio installation.
.PHONY: checkandroidsdk
checkandroidsdk: ## Check that Android SDK is installed
	@$(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager --list_installed | grep -q 'ndk' || (\
		echo -e "\n\tERROR: Android SDK not installed.\n\
		\tANDROID_HOME=$(ANDROID_HOME)\n\
		\tANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT)\n\n\
		See README.md for instructions on how to install the prerequisites.\n"; exit 1)

.PHONY: test
test: gradle-dependencies ## Run the Android tests
	(cd android && ./gradlew test)

.PHONY: emulator 
emulator: ## Start an android emulator instance
	@echo "Checking installed SDK packages..."
	@if ! $(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager --list_installed | grep -q "$(AVD_IMAGE)"; then \
		echo "$(AVD_IMAGE) not found, installing..."; \
		$(ANDROID_HOME)/cmdline-tools/latest/bin/sdkmanager "$(AVD_IMAGE)"; \
	fi
	@echo "Checking if AVD exists..."
	@if ! $(ANDROID_HOME)/cmdline-tools/latest/bin/avdmanager list avd | grep -q "$(AVD)"; then \
		echo "AVD $(AVD) not found, creating..."; \
		$(ANDROID_HOME)/cmdline-tools/latest/bin/avdmanager create avd -n "$(AVD)" -k "$(AVD_IMAGE)"; \
	fi
	@echo "Starting emulator..."
	@$(ANDROID_HOME)/emulator/emulator -avd "$(AVD)" -logcat-output /dev/stdout -netdelay none -netspeed full

.PHONY: install 
install: $(DEBUG_APK) ## Install the debug APK on a connected device
	adb install -r $<

.PHONY: run
run: install ## Run the debug APK on a connected device
	adb shell am start -n com.tailscale.ipn/com.tailscale.ipn.MainActivity

.PHONY: docker-build-image 
docker-build-image: ## Builds the docker image for the android build environment if it does not exist
	@echo "Checking if docker image $(DOCKER_IMAGE) already exists..."
	@if ! docker images $(DOCKER_IMAGE) -q | grep -q . ; then \
		echo "Image does not exist. Building..."; \
		docker build -f docker/DockerFile.amd64-build -t $(DOCKER_IMAGE) .; \
	fi

.PHONY: docker-run-build
docker-run-build: clean jarsign-env docker-build-image ## Runs the docker image for the android build environment and builds release
	@docker run --rm -v $(CURDIR):/build/tailscale-android --env JKS_PASSWORD=$(JKS_PASSWORD) --env JKS_PATH=$(JKS_PATH) $(DOCKER_IMAGE)

.PHONY: docker-remove-build-image
docker-remove-build-image: ## Removes the current docker build image
	docker rmi --force $(DOCKER_IMAGE)

.PHONY: docker-all ## Makes a fresh docker environment, builds docker and cleans up. For CI.
docker-all: docker-build-image docker-run-build $(DOCKER_IMAGE)

.PHONY: docker-shell
docker-shell: docker-build-image ## Builds a docker image with the android build env and opens a shell
	docker run --rm -v $(CURDIR):/build/tailscale-android -it $(DOCKER_IMAGE) /bin/bash

.PHONY: docker-remove-shell-image
docker-remove-shell-image: ## Removes all docker shell image
	@echo "docker-remove-shell-image retained for backward compatibility, but is a no-op; docker-shell now uses build image"

.PHONY: clean
clean: ## Remove build artifacts. Does not purge docker build envs. Use dockerRemoveEnv for that.
	@echo "Cleaning up old build artifacts"
	-rm -rf android/build $(DEBUG_APK) $(RELEASE_APK) $(RELEASE_AAB) $(RELEASE_TV_AAB) $(LIBTAILSCALE_AAR) android/libs *.apk *.aab
	@echo "Cleaning cached toolchain"
	-rm -rf $(HOME)/.cache/tailscale-go{,.extracted}
	-pkill -f gradle
	-rm tailscale.version

.PHONY: help
help: ## Show this help
	@echo "\nSpecify a command. The choices are:\n"
	@grep -hE '^[0-9a-zA-Z_-]+:.*?## .*$$' ${MAKEFILE_LIST} | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[0;36m%-20s\033[m %s\n", $$1, $$2}'
	@echo ""

.DEFAULT_GOAL := help