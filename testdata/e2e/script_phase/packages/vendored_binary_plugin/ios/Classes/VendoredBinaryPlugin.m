#import "VendoredBinaryPlugin.h"
#import "StaticThing.h"
#import "ScriptPhaseGenerated.h"
@import DynamicThing;

@implementation VendoredBinaryPlugin
+ (void)registerWithRegistrar:(NSObject<FlutterPluginRegistrar>*)registrar {
  const int value = DynamicThingValue() + StaticThingValue() + SCRIPT_PHASE_VALUE;
  if (value != 49) {
    __builtin_trap();
  }
}
@end
