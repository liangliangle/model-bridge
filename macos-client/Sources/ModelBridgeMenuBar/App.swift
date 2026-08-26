import SwiftUI

@main
struct ModelBridgeMenuBarApp: App {
    @StateObject private var viewModel = MenuBarViewModel()

    var body: some Scene {
        MenuBarExtra {
            MainView(vm: viewModel)
        } label: {
            HStack(spacing: 4) {
                Image(systemName: "bolt.fill")
                    .foregroundColor(viewModel.isConnected ? .yellow : .gray)
                Text(viewModel.formattedTotalTokens)
                    .font(.system(.body, design: .monospaced))
            }
        }
        .menuBarExtraStyle(.window)
    }
}
