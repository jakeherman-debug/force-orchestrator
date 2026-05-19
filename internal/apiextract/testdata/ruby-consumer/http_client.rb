# Ruby HTTP client fixture for D15 P5 consumer extractor tests.

require 'httparty'
require 'faraday'
require 'rest-client'

class UserService
  # HTTParty calls
  def list_users
    HTTParty.get('https://api.example.com/users')
  end

  def create_user(params)
    HTTParty.post('/api/users', body: params)
  end

  def delete_user(id)
    HTTParty.delete('https://api.example.com/users/' + id.to_s)
  end

  # Faraday connection call
  def update_user(id, params)
    conn = Faraday.new(url: 'https://api.example.com')
    conn.put('/api/users/profile', params)
  end

  # Faraday class-method call
  def get_profile
    Faraday.get('/api/profile')
  end

  # Net::HTTP call
  def fetch_orders
    Net::HTTP.get(URI('https://api.example.com/orders'))
  end

  # RestClient call
  def get_settings
    RestClient.get('https://api.example.com/settings')
  end
end
