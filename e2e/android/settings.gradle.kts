// The ExoPlayer exit suite: a Media3 instrumented test against
// TestPlayerFixture, reached from the emulator through `adb reverse`.
pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    repositories {
        google()
        mavenCentral()
    }
}
rootProject.name = "clipmux-exit"
include(":app")
