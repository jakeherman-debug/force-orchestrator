Rails.application.routes.draw do
  # Direct route declarations
  get  '/users/:id', to: 'users#show'
  post '/users',     to: 'users#create'
  delete '/users/:id', to: 'users#destroy'

  # Resources expansion
  resources :articles

  # Namespace block
  namespace :api do
    resources :posts
  end

  # Root declaration
  root 'home#index'

  # Match with via
  match '/search', via: [:get, :post]
end
