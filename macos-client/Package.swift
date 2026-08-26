// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "ModelBridgeMenuBar",
    platforms: [
        .macOS(.v13)
    ],
    products: [
        .executable(name: "ModelBridgeMenuBar", targets: ["ModelBridgeMenuBar"])
    ],
    dependencies: [],
    targets: [
        .executableTarget(
            name: "ModelBridgeMenuBar",
            dependencies: [],
            path: "Sources/ModelBridgeMenuBar"
        )
    ]
)
