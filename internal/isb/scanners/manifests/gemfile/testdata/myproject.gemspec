Gem::Specification.new do |s|
  s.name = "myproject"
  s.version = "0.1.0"
  s.summary = "Demo gem"
  s.add_dependency "activesupport", "~> 7.0"
  s.add_runtime_dependency "json", "2.6.3"
  s.add_development_dependency "rspec", "~> 3.12"
end
