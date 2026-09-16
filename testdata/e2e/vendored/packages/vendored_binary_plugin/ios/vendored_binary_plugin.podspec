Pod::Spec.new do |s|
  s.name = 'vendored_binary_plugin'
  s.version = '0.0.1'
  s.summary = 'Synthetic fixture for vendored binary support.'
  s.description = 'Exercises dynamic XCFramework and static vendored library support.'
  s.homepage = 'https://example.invalid'
  s.license = { :type => 'Apache-2.0' }
  s.author = { 'ripley' => 'ripley@example.invalid' }
  s.source = { :path => '.' }
  s.source_files = 'Classes/**/*.{h,m}'
  s.public_header_files = 'Classes/**/*.h'
  s.platform = :ios, '15.0'
  s.dependency 'Flutter'
  s.dependency 'GoogleUtilities/Environment'
  s.vendored_frameworks = 'Vendor/DynamicThing.xcframework'
  s.vendored_libraries = 'Vendor/libStaticThing.a'
  s.static_framework = true
end
