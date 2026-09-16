#import "VendoredBinaryPlugin.h"
#import "StaticThing.h"
@import DynamicThing;

@implementation VendoredBinaryPlugin
+ (void)registerWithRegistrar:(NSObject<FlutterPluginRegistrar>*)registrar {
  const int value = DynamicThingValue() + StaticThingValue();
  if (value != 42) {
    __builtin_trap();
  }
}
@end
