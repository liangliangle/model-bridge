#!/bin/bash
set -e

# 获取脚本所在目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

APP_NAME="Model Bridge MenuBar"
BUNDLE_ID="com.modelbridge.menubar"
VERSION="1.0.0"
DIST_DIR="$SCRIPT_DIR/dist"
RELEASE_DIR="$ROOT_DIR/release"
STAGING_DIR="/tmp/modelbridge_app_staging"
DMG_STAGING="/tmp/modelbridge_dmg_staging"

echo "=========================================="
echo "🚀 开始构建 $APP_NAME.app 与 .dmg 安装包"
echo "=========================================="

# 1. 编译 Release 二进制
echo "📦 [1/5] 编译 Swift Release 二进制..."
cd "$SCRIPT_DIR"
swift build -c release

BIN_PATH="$SCRIPT_DIR/.build/release/ModelBridgeMenuBar"
if [ ! -f "$BIN_PATH" ]; then
  echo "❌ 找不到编译产物: $BIN_PATH"
  exit 1
fi

# 2. 准备图标
echo "🎨 [2/5] 准备应用图标 AppIcon.icns..."
if [ ! -f "$SCRIPT_DIR/AppIcon.icns" ]; then
  if [ -f "$ROOT_DIR/public/icon-512.png" ]; then
    ICONSET_DIR="/tmp/AppIcon.iconset"
    mkdir -p "$ICONSET_DIR"
    sips -z 16 16     "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_16x16.png" >/dev/null 2>&1
    sips -z 32 32     "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_16x16@2x.png" >/dev/null 2>&1
    sips -z 32 32     "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_32x32.png" >/dev/null 2>&1
    sips -z 64 64     "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_32x32@2x.png" >/dev/null 2>&1
    sips -z 128 128   "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_128x128.png" >/dev/null 2>&1
    sips -z 256 256   "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_128x128@2x.png" >/dev/null 2>&1
    sips -z 256 256   "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_256x256.png" >/dev/null 2>&1
    sips -z 512 512   "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_256x256@2x.png" >/dev/null 2>&1
    sips -z 512 512   "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_512x512.png" >/dev/null 2>&1
    sips -z 1024 1024 "$ROOT_DIR/public/icon-512.png" --out "$ICONSET_DIR/icon_512x512@2x.png" >/dev/null 2>&1
    iconutil -c icns "$ICONSET_DIR" -o "$SCRIPT_DIR/AppIcon.icns"
    rm -rf "$ICONSET_DIR"
  fi
fi

# 3. 组装 .app Bundle
echo "📂 [3/5] 组装 $APP_NAME.app 目录结构..."
rm -rf "$STAGING_DIR"
mkdir -p "$STAGING_DIR/$APP_NAME.app/Contents/MacOS"
mkdir -p "$STAGING_DIR/$APP_NAME.app/Contents/Resources"

# 复制二进制
cp "$BIN_PATH" "$STAGING_DIR/$APP_NAME.app/Contents/MacOS/ModelBridgeMenuBar"
chmod +x "$STAGING_DIR/$APP_NAME.app/Contents/MacOS/ModelBridgeMenuBar"

# 复制图标
if [ -f "$SCRIPT_DIR/AppIcon.icns" ]; then
  cp "$SCRIPT_DIR/AppIcon.icns" "$STAGING_DIR/$APP_NAME.app/Contents/Resources/AppIcon.icns"
fi

# 写入 PkgInfo
echo -n "APPL????" > "$STAGING_DIR/$APP_NAME.app/Contents/PkgInfo"

# 写入 Info.plist (包含 LSUIElement=true 隐藏 Dock 图标纯常驻菜单栏)
cat << 'EOF' > "$STAGING_DIR/$APP_NAME.app/Contents/Info.plist"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>ModelBridgeMenuBar</string>
    <key>CFBundleIdentifier</key>
    <string>com.modelbridge.menubar</string>
    <key>CFBundleName</key>
    <string>Model Bridge</string>
    <key>CFBundleDisplayName</key>
    <string>Model Bridge MenuBar</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleInfoDictionaryVersion</key>
    <string>6.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0.0</string>
    <key>CFBundleVersion</key>
    <string>1.0.0</string>
    <key>CFBundleIconFile</key>
    <string>AppIcon</string>
    <key>LSMinimumSystemVersion</key>
    <string>13.0</string>
    <key>LSUIElement</key>
    <true/>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>NSPrincipalClass</key>
    <string>NSApplication</string>
    <key>NSAppTransportSecurity</key>
    <dict>
        <key>NSAllowsArbitraryLoads</key>
        <true/>
    </dict>
</dict>
</plist>
EOF

# 代码自签名
echo "🔏 代码自签名 (Ad-hoc signing)..."
codesign --force --deep --sign - "$STAGING_DIR/$APP_NAME.app"

# 4. 生成目标输出目录
mkdir -p "$DIST_DIR"
mkdir -p "$RELEASE_DIR"

rm -rf "$DIST_DIR/$APP_NAME.app"
cp -R "$STAGING_DIR/$APP_NAME.app" "$DIST_DIR/$APP_NAME.app"

rm -rf "$RELEASE_DIR/$APP_NAME.app"
cp -R "$STAGING_DIR/$APP_NAME.app" "$RELEASE_DIR/$APP_NAME.app"

echo "✅ App 构建成功: $RELEASE_DIR/$APP_NAME.app"

# 5. 打包成 DMG
echo "💿 [5/5] 生成 DMG 安装镜像..."
rm -rf "$DMG_STAGING"
mkdir -p "$DMG_STAGING"

# 复制 app 到 DMG 暂存区
cp -R "$STAGING_DIR/$APP_NAME.app" "$DMG_STAGING/$APP_NAME.app"

# 创建 Applications 软链接以支持拖拽安装
ln -s /Applications "$DMG_STAGING/Applications"

DMG_NAME="ModelBridge-MenuBar-v${VERSION}-macOS.dmg"
DMG_OUTPUT="$RELEASE_DIR/$DMG_NAME"

rm -f "$DMG_OUTPUT"
rm -f "$DIST_DIR/$DMG_NAME"

hdiutil create -volname "$APP_NAME" \
  -srcfolder "$DMG_STAGING" \
  -ov -format UDZO \
  "$DMG_OUTPUT"

if [ -f "$DMG_OUTPUT" ]; then
  cp -f "$DMG_OUTPUT" "$DIST_DIR/$DMG_NAME" 2>/dev/null || true
fi

# 清理临时文件
rm -rf "$STAGING_DIR"
rm -rf "$DMG_STAGING"

echo "=========================================="
echo "🎉 构建全部完成！产物路径如下："
echo "   📱 APP 程序: $RELEASE_DIR/$APP_NAME.app"
echo "   💿 DMG 安装包: $RELEASE_DIR/$DMG_NAME"
echo "=========================================="
